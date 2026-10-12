// Package receiver implements the receiver's request pipeline (§6.1 of the
// architecture) as a plain http.Handler.
//
// The receiver writes one log line per request. It never logs bodies, query
// strings, header values, route IDs, secrets or target URLs (invariant 7).
// The only value it logs from a request is the delivery ID, and only after
// the request passed verification.
package receiver

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/NiekKeijzer/webhook-relay/accept"
	"github.com/NiekKeijzer/webhook-relay/envelope"
	"github.com/NiekKeijzer/webhook-relay/plugin"
	"github.com/NiekKeijzer/webhook-relay/queue"
	"github.com/NiekKeijzer/webhook-relay/verify"
)

// DefaultSendTimeout bounds the queue sends of one request, so the receiver
// answers within GitHub's 10-second deadline.
const DefaultSendTimeout = 8 * time.Second

// maxDeliveryIDLen is the longest delivery ID taken from a sender.
const maxDeliveryIDLen = 256

// defaultHeaders are kept on every route (§6.5).
var defaultHeaders = []string{"Content-Type", "User-Agent"}

const reservedHeaderPrefix = "X-Relay-"

// Route is one configured route as the receiver uses it.
type Route struct {
	Name     string
	IDs      []string
	Methods  []string
	Verifier verify.Verifier
	Acceptor accept.Acceptor
	// DeliveryIDHeader carries the sender's delivery ID; empty if none.
	DeliveryIDHeader string
	// Headers are kept in addition to the default set and the verifier's.
	Headers []string
	// Targets are the names of the route's targets.
	Targets []string
}

// Options configures a Handler.
type Options struct {
	Routes []Route
	Queue  queue.Sender
	// Key signs envelopes: the current envelope key.
	Key []byte
	// MaxBodyBytes is the largest request body the receiver reads.
	MaxBodyBytes int64
	// SendTimeout bounds the queue sends of one request. Zero means
	// DefaultSendTimeout.
	SendTimeout time.Duration
	// Logger receives one line per request. Nil means slog.Default().
	Logger *slog.Logger
}

// Handler is the receiver's http.Handler.
type Handler struct {
	routes      map[string]*route // by route ID
	queue       queue.Sender
	key         []byte
	maxBody     int64
	sendTimeout time.Duration
	log         *slog.Logger
}

type route struct {
	Route
	keep  []string // canonical names of the headers to keep
	allow string   // value of the Allow header
}

// New checks opts and returns a Handler.
func New(opts Options) (*Handler, error) {
	h := &Handler{
		routes:      map[string]*route{},
		queue:       opts.Queue,
		key:         opts.Key,
		maxBody:     opts.MaxBodyBytes,
		sendTimeout: opts.SendTimeout,
		log:         opts.Logger,
	}
	switch {
	case h.queue == nil:
		return nil, errors.New("receiver: no queue")
	case len(h.key) == 0:
		return nil, errors.New("receiver: no envelope key")
	case h.maxBody <= 0:
		return nil, errors.New("receiver: MaxBodyBytes must be positive")
	}
	if h.sendTimeout == 0 {
		h.sendTimeout = DefaultSendTimeout
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	for _, r := range opts.Routes {
		switch {
		case r.Name == "":
			return nil, errors.New("receiver: route without a name")
		case len(r.IDs) == 0, len(r.Methods) == 0, len(r.Targets) == 0:
			return nil, fmt.Errorf("receiver: route %q needs IDs, methods and targets", r.Name)
		case r.Verifier == nil || r.Acceptor == nil:
			return nil, fmt.Errorf("receiver: route %q needs a verifier and an acceptor", r.Name)
		}
		rt := &route{Route: r, allow: strings.Join(r.Methods, ", ")}
		for _, name := range slices.Concat(defaultHeaders, r.Verifier.Headers(), r.Headers) {
			name = http.CanonicalHeaderKey(name)
			if strings.HasPrefix(name, reservedHeaderPrefix) {
				return nil, fmt.Errorf("receiver: route %q: header %s is reserved for the forwarder", r.Name, name)
			}
			if !slices.Contains(rt.keep, name) {
				rt.keep = append(rt.keep, name)
			}
		}
		for _, id := range r.IDs {
			if _, ok := h.routes[id]; ok {
				return nil, fmt.Errorf("receiver: route %q: an ID is used by more than one route", r.Name)
			}
			h.routes[id] = rt
		}
	}
	return h, nil
}

// result describes the handling of one request for its log line.
type result struct {
	outcome    string
	route      string
	deliveryID string
	status     int
	bodyBytes  int
	envelopes  int
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	res := h.handle(w, r)
	h.log.LogAttrs(r.Context(), slog.LevelInfo, res.outcome,
		slog.String("route", res.route),
		slog.String("delivery_id", res.deliveryID),
		slog.Int("status", res.status),
		slog.Int("body_bytes", res.bodyBytes),
		slog.Int("envelopes", res.envelopes),
		slog.Duration("duration", time.Since(start)),
	)
}

func (h *Handler) handle(w http.ResponseWriter, r *http.Request) result {
	res := result{route: "unknown"}
	// Error responses have an empty body (invariant 9).
	fail := func(status int, outcome string) result {
		w.WriteHeader(status)
		res.status, res.outcome = status, outcome
		return res
	}

	// 1. Match the path to a route ID. The path must be exactly /<id>.
	path := r.URL.EscapedPath()
	rt := h.routes[strings.TrimPrefix(path, "/")]
	if rt == nil || !strings.HasPrefix(path, "/") {
		return fail(http.StatusNotFound, "unknown route")
	}
	res.route = rt.Name

	// 2. Check the method.
	if !slices.Contains(rt.Methods, r.Method) {
		w.Header().Set("Allow", rt.allow)
		return fail(http.StatusMethodNotAllowed, "method not allowed")
	}

	// 3. Read the raw body.
	if r.ContentLength > h.maxBody {
		return fail(http.StatusRequestEntityTooLarge, "body too large")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	res.bodyBytes = len(body)
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return fail(http.StatusRequestEntityTooLarge, "body too large")
		}
		return fail(http.StatusBadRequest, "body read failed")
	}
	req := &plugin.Request{Method: r.Method, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body}

	// 4. Run the verifier. Its error is not logged.
	if rt.Verifier.Verify(req) != nil {
		return fail(http.StatusUnauthorized, "verification failed")
	}
	deliveryID, generated := rt.deliveryID(r.Header)
	res.deliveryID = deliveryID

	// 5. Run the acceptor.
	d := rt.Acceptor.Accept(req)
	if d.Status < 200 || d.Status > 599 || d.Enqueue && d.Status > 299 {
		return fail(http.StatusInternalServerError, "invalid acceptor decision")
	}
	if !d.Enqueue {
		return reply(w, d, res, "replied without enqueueing")
	}

	// 6. Build one envelope per target, and check every size before sending.
	header := http.Header{}
	for _, name := range rt.keep {
		if values := r.Header.Values(name); len(values) > 0 {
			header[name] = slices.Clone(values)
		}
	}
	acceptedAt := time.Now()
	var msgs [][]byte
	for _, target := range rt.Targets {
		msg, err := envelope.Seal(&envelope.Envelope{
			Route:               rt.Name,
			Target:              target,
			DeliveryID:          deliveryID,
			DeliveryIDGenerated: generated,
			ReceivedAt:          acceptedAt,
			Method:              r.Method,
			Query:               r.URL.RawQuery,
			Header:              header,
			Body:                body,
		}, h.key)
		if err != nil {
			return fail(http.StatusInternalServerError, "envelope failed")
		}
		if len(msg) > h.queue.MaxMessageSize() {
			return fail(http.StatusRequestEntityTooLarge, "envelope too large")
		}
		msgs = append(msgs, msg)
	}

	// 7. Send all envelopes, within the send timeout.
	ctx, cancel := context.WithTimeout(r.Context(), h.sendTimeout)
	defer cancel()
	for _, msg := range msgs {
		if err := h.queue.Send(ctx, msg); err != nil {
			return fail(http.StatusServiceUnavailable, "send failed")
		}
		res.envelopes++
	}

	// 8. Send the acceptor's reply.
	return reply(w, d, res, "enqueued")
}

// reply sends an acceptor's reply. A reply other than 2xx goes out with an
// empty body (invariant 9).
func reply(w http.ResponseWriter, d accept.Decision, res result, outcome string) result {
	for name, values := range d.Header {
		w.Header()[name] = slices.Clone(values)
	}
	body := d.Body
	if d.Status > 299 {
		body = nil
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(d.Status)
	w.Write(body)
	res.status, res.outcome = d.Status, outcome
	return res
}

// deliveryID returns the sender's delivery ID if the route has a delivery ID
// header and the request carries exactly one usable value. Otherwise it
// generates one.
func (rt *route) deliveryID(h http.Header) (id string, generated bool) {
	if rt.DeliveryIDHeader != "" {
		if values := h.Values(rt.DeliveryIDHeader); len(values) == 1 && usableDeliveryID(values[0]) {
			return values[0], false
		}
	}
	return newUUID(), true
}

// usableDeliveryID reports whether s is non-empty, at most maxDeliveryIDLen
// bytes and printable ASCII.
func usableDeliveryID(s string) bool {
	if s == "" || len(s) > maxDeliveryIDLen {
		return false
	}
	for _, c := range []byte(s) {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// newUUID returns a random version 4 UUID.
func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
