package receiver

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/NiekKeijzer/webhook-relay/accept"
	"github.com/NiekKeijzer/webhook-relay/envelope"
	"github.com/NiekKeijzer/webhook-relay/internal/queuefake"
	"github.com/NiekKeijzer/webhook-relay/plugin"
)

const (
	routeID    = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	rotatedID  = "16fd2706-8baf-433b-82eb-8c7fada847da"
	otherID    = "0f8fad5b-d9cb-469f-a165-70867728950e"
	sigHeader  = "X-Test-Signature"
	validSig   = "sentinel-signature"
	deliveryID = "72d3162e-cc78-11e3-81ab-4c9367dc0958"
)

var key = bytes.Repeat([]byte("k"), 32)

// testVerifier accepts requests whose sigHeader is validSig.
type testVerifier struct{ headers []string }

func (v testVerifier) Verify(req *plugin.Request) error {
	if req.Header.Get(sigHeader) != validSig {
		return errors.New("bad signature")
	}
	return nil
}

func (v testVerifier) Headers() []string {
	if v.headers != nil {
		return v.headers
	}
	return []string{sigHeader}
}

type acceptFunc func(*plugin.Request) accept.Decision

func (f acceptFunc) Accept(req *plugin.Request) accept.Decision { return f(req) }

func enqueue(status int) acceptFunc {
	return func(*plugin.Request) accept.Decision { return accept.Decision{Enqueue: true, Status: status} }
}

func testRoute() Route {
	return Route{
		Name:             "github-infra",
		IDs:              []string{routeID, rotatedID},
		Methods:          []string{"POST", "PUT"},
		Verifier:         testVerifier{},
		Acceptor:         enqueue(http.StatusAccepted),
		DeliveryIDHeader: "X-Delivery",
		Headers:          []string{"x-event"},
		Targets:          []string{"deploy", "notify"},
	}
}

type setup struct {
	handler *Handler
	queue   *queuefake.Queue
	logs    *bytes.Buffer
}

func newSetup(t *testing.T, change func(*Options), qopts queuefake.Options) setup {
	t.Helper()
	q := queuefake.New(qopts)
	logs := &bytes.Buffer{}
	opts := Options{
		Routes:       []Route{testRoute()},
		Queue:        q,
		Key:          key,
		MaxBodyBytes: 1024,
		Logger:       slog.New(slog.NewJSONHandler(logs, nil)),
	}
	if change != nil {
		change(&opts)
	}
	h, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return setup{handler: h, queue: q, logs: logs}
}

// signed returns a valid request to the test route.
func signed(method, target, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set(sigHeader, validSig)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Delivery", deliveryID)
	return r
}

func (s setup) serve(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handler.ServeHTTP(w, r)
	return w
}

func TestResponses(t *testing.T) {
	big := strings.Repeat("x", 1025)
	random := make([]byte, 1000)
	rand.Read(random)
	boom := errors.New("queue down")
	tests := []struct {
		name      string
		change    func(*Options)
		qopts     queuefake.Options
		hook      func(ctx context.Context, n int) error
		req       func() *http.Request
		status    int
		body      string
		sent      int
		allow     string
		maxTimeMs int64
	}{
		{name: "enqueued", req: func() *http.Request { return signed("POST", "/"+routeID, "{}") }, status: 202, sent: 2},
		{name: "second ID of the route", req: func() *http.Request { return signed("POST", "/"+rotatedID, "{}") }, status: 202, sent: 2},
		{name: "second method of the route", req: func() *http.Request { return signed("PUT", "/"+routeID, "{}") }, status: 202, sent: 2},
		{name: "with query string", req: func() *http.Request { return signed("POST", "/"+routeID+"?a=1", "{}") }, status: 202, sent: 2},
		{
			name:   "acceptor reply after enqueueing",
			change: func(o *Options) { o.Routes[0].Acceptor = replyWith(true, 200, "ok") },
			req:    func() *http.Request { return signed("POST", "/"+routeID, "{}") },
			status: 200, body: "ok", sent: 2,
		},

		// 404: the path must be exactly /<id>.
		{name: "root", req: func() *http.Request { return signed("POST", "/", "{}") }, status: 404},
		{name: "unknown ID", req: func() *http.Request { return signed("POST", "/"+otherID, "{}") }, status: 404},
		{name: "trailing slash", req: func() *http.Request { return signed("POST", "/"+routeID+"/", "{}") }, status: 404},
		{name: "prefixed path", req: func() *http.Request { return signed("POST", "/hooks/"+routeID, "{}") }, status: 404},
		{name: "uppercase ID", req: func() *http.Request { return signed("POST", "/"+strings.ToUpper(routeID), "{}") }, status: 404},
		{name: "percent-encoded ID", req: func() *http.Request { return signed("POST", "/%37"+routeID[1:], "{}") }, status: 404},

		// 405
		{name: "wrong method", req: func() *http.Request { return signed("GET", "/"+routeID, "") }, status: 405, allow: "POST, PUT"},

		// 413: body over the limit.
		{name: "body over limit", req: func() *http.Request { return signed("POST", "/"+routeID, big) }, status: 413},
		{name: "chunked body over limit", req: func() *http.Request {
			r := signed("POST", "/"+routeID, "")
			r.Body = io.NopCloser(strings.NewReader(big))
			r.ContentLength = -1
			return r
		}, status: 413},

		// 401
		{name: "bad signature", req: func() *http.Request {
			r := signed("POST", "/"+routeID, "{}")
			r.Header.Set(sigHeader, "forged")
			return r
		}, status: 401},
		{name: "missing signature", req: func() *http.Request {
			r := signed("POST", "/"+routeID, "{}")
			r.Header.Del(sigHeader)
			return r
		}, status: 401},

		// The acceptor replies without enqueueing.
		{
			name:   "handshake reply",
			change: func(o *Options) { o.Routes[0].Acceptor = replyWith(false, 200, "pong") },
			req:    func() *http.Request { return signed("POST", "/"+routeID, "{}") },
			status: 200, body: "pong",
		},
		{
			name:   "error reply loses its body",
			change: func(o *Options) { o.Routes[0].Acceptor = replyWith(false, 403, "detail") },
			req:    func() *http.Request { return signed("POST", "/"+routeID, "{}") },
			status: 403,
		},

		// 500: invalid acceptor decisions.
		{
			name:   "enqueue with an error status",
			change: func(o *Options) { o.Routes[0].Acceptor = replyWith(true, 400, "detail") },
			req:    func() *http.Request { return signed("POST", "/"+routeID, "{}") },
			status: 500,
		},
		{
			name:   "no status",
			change: func(o *Options) { o.Routes[0].Acceptor = replyWith(false, 0, "") },
			req:    func() *http.Request { return signed("POST", "/"+routeID, "{}") },
			status: 500,
		},

		// 413: an envelope over the queue's maximum; nothing is enqueued.
		{
			name:   "envelope over queue maximum",
			qopts:  queuefake.Options{MaxMessageSize: 1100},
			change: func(o *Options) { o.MaxBodyBytes = 1 << 20 },
			req:    func() *http.Request { return signed("POST", "/"+routeID, string(random)) },
			status: 413,
		},

		// 503
		{name: "send fails", hook: failOn(1, boom), req: func() *http.Request { return signed("POST", "/"+routeID, "{}") }, status: 503},
		{name: "second send fails", hook: failOn(2, boom), req: func() *http.Request { return signed("POST", "/"+routeID, "{}") }, status: 503, sent: 1},
		{
			name:   "send times out",
			change: func(o *Options) { o.SendTimeout = 50 * time.Millisecond },
			hook: func(ctx context.Context, _ int) error {
				<-ctx.Done()
				return ctx.Err()
			},
			req:       func() *http.Request { return signed("POST", "/"+routeID, "{}") },
			status:    503,
			maxTimeMs: 2000,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSetup(t, tt.change, tt.qopts)
			if tt.hook != nil {
				s.queue.SetSendHook(tt.hook)
			}
			start := time.Now()
			w := s.serve(tt.req())
			if w.Code != tt.status {
				t.Errorf("status = %d, want %d", w.Code, tt.status)
			}
			if got := w.Body.String(); got != tt.body {
				t.Errorf("body = %q, want %q", got, tt.body)
			}
			if got := s.queue.Sent(); got != tt.sent {
				t.Errorf("sent %d envelopes, want %d", got, tt.sent)
			}
			if got := w.Header().Get("Allow"); got != tt.allow {
				t.Errorf("Allow = %q, want %q", got, tt.allow)
			}
			if tt.maxTimeMs > 0 && time.Since(start).Milliseconds() > tt.maxTimeMs {
				t.Errorf("answered after %v", time.Since(start))
			}
		})
	}
}

func replyWith(enqueue bool, status int, body string) acceptFunc {
	return func(*plugin.Request) accept.Decision {
		return accept.Decision{Enqueue: enqueue, Status: status, Header: http.Header{"X-Reply": {"yes"}}, Body: []byte(body)}
	}
}

func failOn(n int, err error) func(context.Context, int) error {
	return func(_ context.Context, i int) error {
		if i == n {
			return err
		}
		return nil
	}
}

func TestAcceptorHeaders(t *testing.T) {
	s := newSetup(t, func(o *Options) { o.Routes[0].Acceptor = replyWith(false, 200, "pong") }, queuefake.Options{})
	w := s.serve(signed("POST", "/"+routeID, "{}"))
	if w.Header().Get("X-Reply") != "yes" {
		t.Errorf("header = %v", w.Header())
	}
}

func TestEnvelopes(t *testing.T) {
	s := newSetup(t, nil, queuefake.Options{})
	body := "{\"action\":\"opened\"}\x00\xff"
	r := signed("POST", "/"+routeID+"?b=2&a=%201&b=1", body)
	r.Header.Set("User-Agent", "GitHub-Hookshot/abc")
	r.Header.Add("X-Event", "push")
	r.Header.Add("X-Event", "ping")
	r.Header.Set("X-Other", "dropped")
	r.Header.Set("Authorization", "Bearer dropped")
	before := time.Now()
	if w := s.serve(r); w.Code != 202 {
		t.Fatalf("status = %d", w.Code)
	}

	bodies := s.queue.Bodies()
	if len(bodies) != 2 {
		t.Fatalf("got %d envelopes", len(bodies))
	}
	var targets []string
	for _, msg := range bodies {
		e, err := envelope.Open(msg, [][]byte{key})
		if err != nil {
			t.Fatal(err)
		}
		targets = append(targets, e.Target)
		if e.Route != "github-infra" || e.DeliveryID != deliveryID || e.DeliveryIDGenerated {
			t.Errorf("envelope = %+v", e)
		}
		if e.Method != "POST" || e.Query != "b=2&a=%201&b=1" || string(e.Body) != body {
			t.Errorf("request changed: %s %q %q", e.Method, e.Query, e.Body)
		}
		if e.ReceivedAt.Before(before) || e.ReceivedAt.After(time.Now()) {
			t.Errorf("received at %v", e.ReceivedAt)
		}
		want := http.Header{
			"Content-Type":     {"application/json"},
			"User-Agent":       {"GitHub-Hookshot/abc"},
			"X-Test-Signature": {validSig},
			"X-Event":          {"push", "ping"},
		}
		if !headersEqual(e.Header, want) {
			t.Errorf("header = %v\nwant %v", e.Header, want)
		}
	}
	if !slices.Equal(targets, []string{"deploy", "notify"}) {
		t.Errorf("targets = %v", targets)
	}
	for _, msg := range bodies {
		if bytes.Contains(msg, []byte(routeID)) {
			t.Error("envelope contains the route ID")
		}
	}
}

func headersEqual(a, b http.Header) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !slices.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestDeliveryID(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		noHdr  bool // the route has no delivery ID header
		want   string
	}{
		{name: "from sender", values: []string{"abc-123"}, want: "abc-123"},
		{name: "absent", values: nil},
		{name: "empty", values: []string{""}},
		{name: "repeated", values: []string{"a", "b"}},
		{name: "too long", values: []string{strings.Repeat("a", 257)}},
		{name: "longest", values: []string{strings.Repeat("a", 256)}, want: strings.Repeat("a", 256)},
		{name: "control character", values: []string{"a\x01b"}},
		{name: "non-ASCII", values: []string{"aé"}},
		{name: "route without header", values: []string{"abc-123"}, noHdr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSetup(t, func(o *Options) {
				if tt.noHdr {
					o.Routes[0].DeliveryIDHeader = ""
				}
			}, queuefake.Options{})
			r := signed("POST", "/"+routeID, "{}")
			r.Header.Del("X-Delivery")
			for _, v := range tt.values {
				r.Header.Add("X-Delivery", v)
			}
			if w := s.serve(r); w.Code != 202 {
				t.Fatalf("status = %d", w.Code)
			}
			var ids []string
			for _, msg := range s.queue.Bodies() {
				e, err := envelope.Open(msg, [][]byte{key})
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, e.DeliveryID)
				if e.DeliveryIDGenerated != (tt.want == "") {
					t.Errorf("generated = %v", e.DeliveryIDGenerated)
				}
			}
			if len(ids) != 2 || ids[0] != ids[1] {
				t.Fatalf("delivery IDs %q differ between envelopes", ids)
			}
			if tt.want != "" && ids[0] != tt.want {
				t.Errorf("delivery ID = %q, want %q", ids[0], tt.want)
			}
			if tt.want == "" && !uuidV4.MatchString(ids[0]) {
				t.Errorf("generated delivery ID %q is not a version 4 UUID", ids[0])
			}
		})
	}
}

func TestNewRejects(t *testing.T) {
	q := queuefake.New(queuefake.Options{})
	tests := []struct {
		name   string
		change func(*Options)
	}{
		{"no queue", func(o *Options) { o.Queue = nil }},
		{"no key", func(o *Options) { o.Key = nil }},
		{"no body limit", func(o *Options) { o.MaxBodyBytes = 0 }},
		{"no verifier", func(o *Options) { o.Routes[0].Verifier = nil }},
		{"no acceptor", func(o *Options) { o.Routes[0].Acceptor = nil }},
		{"no targets", func(o *Options) { o.Routes[0].Targets = nil }},
		{"reserved route header", func(o *Options) { o.Routes[0].Headers = []string{"x-relay-attempt"} }},
		{"reserved verifier header", func(o *Options) { o.Routes[0].Verifier = testVerifier{headers: []string{"X-Relay-Delivery-Id"}} }},
		{"ID on two routes", func(o *Options) {
			other := testRoute()
			other.Name = "other"
			other.IDs = []string{routeID}
			o.Routes = append(o.Routes, other)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := Options{Routes: []Route{testRoute()}, Queue: q, Key: key, MaxBodyBytes: 1024}
			tt.change(&opts)
			_, err := New(opts)
			if err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(err.Error(), routeID) {
				t.Errorf("error %q contains the route ID", err)
			}
		})
	}
}
