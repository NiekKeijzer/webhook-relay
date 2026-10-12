// Package forwarder receives envelopes from the queue, checks them, and
// delivers the captured requests to their targets (§9.1 of the
// architecture).
//
// The forwarder writes one log line per message. It never logs bodies, query
// strings, header values, secrets or target URLs, and it logs no error text
// from HTTP calls, because that text contains the target's URL and address
// (invariant 7).
package forwarder

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/NiekKeijzer/webhook-relay/envelope"
	"github.com/NiekKeijzer/webhook-relay/queue"
)

const (
	// dedupeMaxEntries bounds the dedupe store.
	dedupeMaxEntries = 10_000
	// receiveRetryDelay is the pause after a failed Receive.
	receiveRetryDelay = 5 * time.Second
	// ackTimeout bounds one Ack call.
	ackTimeout = 10 * time.Second
	// maxDrain is how much of a target's response body is read before the
	// connection is reused.
	maxDrain = 64 << 10
)

// Options configures a Forwarder.
type Options struct {
	Queue queue.Receiver
	// Keys are the envelope keys; an envelope signed with any of them is
	// accepted.
	Keys [][]byte
	// Targets maps a route name, then a target name, to the target's URL.
	Targets map[string]map[string]*url.URL
	// Timeout bounds one forward. It must be shorter than the queue's ack
	// deadline.
	Timeout time.Duration
	// DedupeWindow is how long delivered dedupe keys are remembered.
	DedupeWindow time.Duration
	// Logger receives one line per message. Nil means slog.Default().
	Logger *slog.Logger
}

// Forwarder delivers queued requests to targets, one message at a time.
type Forwarder struct {
	queue   queue.Receiver
	keys    [][]byte
	targets map[string]map[string]*url.URL
	client  *http.Client
	dedupe  *dedupe
	log     *slog.Logger
	now     func() time.Time
}

// New checks opts and returns a Forwarder.
func New(opts Options) (*Forwarder, error) {
	switch {
	case opts.Queue == nil:
		return nil, errors.New("forwarder: no queue")
	case len(opts.Keys) == 0:
		return nil, errors.New("forwarder: no envelope keys")
	case opts.Timeout <= 0 || opts.DedupeWindow <= 0:
		return nil, errors.New("forwarder: timeout and dedupe window must be positive")
	case opts.Timeout >= opts.Queue.AckDeadline():
		return nil, errors.New("forwarder: timeout must be shorter than the queue's ack deadline")
	}
	for _, targets := range opts.Targets {
		for name, u := range targets {
			if u == nil || u.RawQuery != "" || u.ForceQuery {
				return nil, errors.New("forwarder: target " + strconv.Quote(name) + " needs a URL without a query string")
			}
		}
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	// The request reaches the target as captured (invariant 4): no proxy,
	// and no Accept-Encoding header added for automatic decompression.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	return &Forwarder{
		queue:   opts.Queue,
		keys:    opts.Keys,
		targets: opts.Targets,
		client: &http.Client{
			Transport: transport,
			Timeout:   opts.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		dedupe: newDedupe(opts.DedupeWindow, dedupeMaxEntries),
		log:    log,
		now:    time.Now,
	}, nil
}

// Run receives and processes messages until ctx is cancelled. Then it stops
// receiving, finishes the message in progress and returns nil. Messages that
// were not acknowledged are redelivered later.
func (f *Forwarder) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		msgs, err := f.queue.Receive(ctx, 1)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			// The error is not logged: a backend's error text is not known
			// to be free of payload data.
			f.log.LogAttrs(ctx, slog.LevelError, "receive failed")
			select {
			case <-ctx.Done():
			case <-time.After(receiveRetryDelay):
			}
			continue
		}
		for _, m := range msgs {
			// Shutdown does not interrupt a forward in progress.
			f.process(context.WithoutCancel(ctx), m)
		}
	}
	return nil
}

// result describes the processing of one message for its log line.
type result struct {
	outcome    string
	ok         bool
	route      string
	target     string
	deliveryID string
	status     int
}

func (f *Forwarder) process(ctx context.Context, m queue.Message) {
	start := f.now()
	res := f.handle(ctx, m)
	attrs := []slog.Attr{
		slog.String("route", res.route),
		slog.String("target", res.target),
		slog.String("delivery_id", res.deliveryID),
		slog.Int("delivery_count", m.DeliveryCount),
	}
	if res.status != 0 {
		attrs = append(attrs, slog.Int("status", res.status))
	}
	attrs = append(attrs, slog.Duration("duration", f.now().Sub(start)))
	level := slog.LevelInfo
	if !res.ok {
		level = slog.LevelWarn
	}
	f.log.LogAttrs(ctx, level, res.outcome, attrs...)
}

func (f *Forwarder) handle(ctx context.Context, m queue.Message) result {
	var res result
	done := func(outcome string) result {
		res.outcome = outcome
		return res
	}

	// 1, 2. Check the signature, then decode. On failure, do not ack.
	env, err := envelope.Open(m.Body, f.keys)
	switch {
	case errors.Is(err, envelope.ErrSignature):
		return done("invalid signature")
	case errors.Is(err, envelope.ErrVersion):
		return done("unknown envelope version")
	case err != nil:
		return done("malformed envelope")
	}
	res.route, res.target, res.deliveryID = env.Route, env.Target, env.DeliveryID

	// 3. Find the target.
	u := f.targets[env.Route][env.Target]
	if u == nil {
		return done("unknown target")
	}

	// 4. Dedupe.
	key := dedupeKey{route: env.Route, target: env.Target, deliveryID: env.DeliveryID}
	if f.dedupe.seen(key, f.now()) {
		if err := f.ack(ctx, m.Handle); err != nil {
			return done("ack failed")
		}
		res.ok = true
		return done("duplicate")
	}

	// 5. Forward.
	status, err := f.forward(ctx, u, env, m.DeliveryCount)
	res.status = status
	switch {
	case err != nil:
		return done(classify(err))
	case status < 200 || status > 299:
		return done("target rejected")
	}

	// 6. Acknowledge a 2xx and remember it.
	f.dedupe.add(key, f.now())
	if err := f.ack(ctx, m.Handle); err != nil {
		return done("ack failed")
	}
	res.ok = true
	return done("delivered")
}

// forward sends the captured request to u and returns the target's status.
func (f *Forwarder) forward(ctx context.Context, u *url.URL, env *envelope.Envelope, attempt int) (int, error) {
	req, err := http.NewRequestWithContext(ctx, env.Method, u.String(), bytes.NewReader(env.Body))
	if err != nil {
		return 0, err
	}
	// The target URL has no query string; the captured one is appended.
	req.URL.RawQuery = env.Query
	req.Header = env.Header.Clone()
	if req.Header == nil {
		req.Header = http.Header{}
	}
	// An empty User-Agent stops the client from adding its own.
	if _, ok := req.Header["User-Agent"]; !ok {
		req.Header["User-Agent"] = []string{""}
	}
	req.Header.Set("X-Relay-Delivery-Id", env.DeliveryID)
	req.Header.Set("X-Relay-Received-At", env.ReceivedAt.UTC().Format(time.RFC3339))
	req.Header.Set("X-Relay-Attempt", strconv.Itoa(attempt))

	resp, err := f.client.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
	resp.Body.Close()
	return resp.StatusCode, nil
}

func (f *Forwarder) ack(ctx context.Context, handle string) error {
	ctx, cancel := context.WithTimeout(ctx, ackTimeout)
	defer cancel()
	return f.queue.Ack(ctx, handle)
}

// classify names a forward error without its text, which contains the
// target's URL and address.
func classify(err error) string {
	if ne, ok := errors.AsType[net.Error](err); errors.Is(err, context.DeadlineExceeded) || ok && ne.Timeout() {
		return "target timeout"
	}
	return "target unreachable"
}
