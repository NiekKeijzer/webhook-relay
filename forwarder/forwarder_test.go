package forwarder

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NiekKeijzer/webhook-relay/envelope"
	"github.com/NiekKeijzer/webhook-relay/internal/queuefake"
	"github.com/NiekKeijzer/webhook-relay/queue"
)

var (
	key      = bytes.Repeat([]byte("k"), 32)
	oldKey   = bytes.Repeat([]byte("o"), 32)
	received = time.Date(2026, 10, 12, 15, 4, 5, 0, time.UTC)
)

// recorded is a request as a target saw it.
type recorded struct {
	method, path, rawQuery string
	header                 http.Header
	body                   []byte
}

// newTarget starts a target that records requests and answers with handle,
// or 204 if handle is nil.
func newTarget(t *testing.T, handle http.HandlerFunc) (*url.URL, chan recorded) {
	t.Helper()
	reqs := make(chan recorded, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reqs <- recorded{method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery, header: r.Header.Clone(), body: body}
		if handle != nil {
			handle(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL + "/hooks/github")
	if err != nil {
		t.Fatal(err)
	}
	return u, reqs
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

type setup struct {
	f    *Forwarder
	q    *queuefake.Queue
	logs *bytes.Buffer
}

func newSetup(t *testing.T, deploy *url.URL, change func(*Options)) setup {
	t.Helper()
	q := queuefake.New(queuefake.Options{AckDeadline: 30 * time.Second})
	logs := &bytes.Buffer{}
	opts := Options{
		Queue:        q,
		Keys:         [][]byte{key, oldKey},
		Targets:      map[string]map[string]*url.URL{"github-infra": {"deploy": deploy}},
		Timeout:      2 * time.Second,
		DedupeWindow: time.Hour,
		Logger:       slog.New(slog.NewJSONHandler(logs, nil)),
	}
	if change != nil {
		change(&opts)
	}
	f, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return setup{f: f, q: q, logs: logs}
}

func testEnvelope() *envelope.Envelope {
	return &envelope.Envelope{
		Route:      "github-infra",
		Target:     "deploy",
		DeliveryID: "72d3162e-cc78-11e3-81ab-4c9367dc0958",
		ReceivedAt: received,
		Method:     "POST",
		Query:      "b=2&a=%201&b=1",
		Header: http.Header{
			"Content-Type":        {"application/json"},
			"X-Hub-Signature-256": {"sha256=abc"},
			"X-Multi":             {"one", "two"},
		},
		Body: []byte("{\"zen\":\"Design for failure.\"}\x00\xff"),
	}
}

func seal(t *testing.T, e *envelope.Envelope, k []byte) []byte {
	t.Helper()
	msg, err := envelope.Seal(e, k)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// enqueue sends msg and processes the next message from the queue.
func (s setup) enqueue(t *testing.T, msg []byte) {
	t.Helper()
	if err := s.q.Send(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.q.Receive(t.Context(), 1)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("Receive = %v, %v", msgs, err)
	}
	s.f.process(t.Context(), msgs[0])
}

func TestForward(t *testing.T) {
	deploy, reqs := newTarget(t, nil)
	s := newSetup(t, deploy, nil)
	e := testEnvelope()
	s.enqueue(t, seal(t, e, key))

	if len(reqs) != 1 {
		t.Fatalf("target got %d requests", len(reqs))
	}
	got := <-reqs
	if got.method != "POST" || got.path != "/hooks/github" || got.rawQuery != e.Query || !bytes.Equal(got.body, e.Body) {
		t.Errorf("request changed: %s %s?%s %q", got.method, got.path, got.rawQuery, got.body)
	}
	for name, values := range e.Header {
		if !slices.Equal(got.header.Values(name), values) {
			t.Errorf("%s = %q, want %q", name, got.header.Values(name), values)
		}
	}
	if got.header.Get("X-Relay-Delivery-Id") != e.DeliveryID {
		t.Errorf("X-Relay-Delivery-Id = %q", got.header.Get("X-Relay-Delivery-Id"))
	}
	if got.header.Get("X-Relay-Received-At") != "2026-10-12T15:04:05Z" {
		t.Errorf("X-Relay-Received-At = %q", got.header.Get("X-Relay-Received-At"))
	}
	if got.header.Get("X-Relay-Attempt") != "1" {
		t.Errorf("X-Relay-Attempt = %q", got.header.Get("X-Relay-Attempt"))
	}
	// Nothing else is added: no User-Agent or Accept-Encoding from the client.
	for _, name := range []string{"User-Agent", "Accept-Encoding"} {
		if v, ok := got.header[name]; ok {
			t.Errorf("forwarder added %s: %q", name, v)
		}
	}
	if s.q.Len() != 0 {
		t.Error("message not acknowledged")
	}
}

func TestForwardKeepsUserAgentAndEmptyQuery(t *testing.T) {
	deploy, reqs := newTarget(t, nil)
	s := newSetup(t, deploy, nil)
	e := testEnvelope()
	e.Header.Set("User-Agent", "GitHub-Hookshot/abc")
	e.Query = ""
	e.Method = "PUT"
	s.enqueue(t, seal(t, e, key))
	got := <-reqs
	if got.header.Get("User-Agent") != "GitHub-Hookshot/abc" || got.rawQuery != "" || got.method != "PUT" {
		t.Errorf("got %s ?%q with User-Agent %q", got.method, got.rawQuery, got.header.Get("User-Agent"))
	}
}

func TestAttemptHeader(t *testing.T) {
	deploy, reqs := newTarget(t, nil)
	s := newSetup(t, deploy, nil)
	s.f.process(t.Context(), queue.Message{Body: seal(t, testEnvelope(), key), DeliveryCount: 3, Handle: "unknown"})
	if got := (<-reqs).header.Get("X-Relay-Attempt"); got != "3" {
		t.Errorf("X-Relay-Attempt = %q, want 3", got)
	}
}

// resign replaces a message's version byte and signs it again, as a holder
// of the key could. It knows the layout: a 32-byte signature, then the
// version.
func resign(msg []byte, version byte) []byte {
	msg = bytes.Clone(msg)
	msg[32] = version
	mac := hmac.New(sha256.New, key)
	mac.Write(msg[32:])
	copy(msg, mac.Sum(nil))
	return msg
}

func TestNotForwarded(t *testing.T) {
	good := seal(t, testEnvelope(), key)
	unknownTarget := testEnvelope()
	unknownTarget.Target = "missing"
	unknownRoute := testEnvelope()
	unknownRoute.Route = "missing"
	tests := []struct {
		name    string
		msg     []byte
		outcome string
	}{
		{"unknown key", seal(t, testEnvelope(), bytes.Repeat([]byte("x"), 32)), "invalid signature"},
		{"no signature", good[32:], "invalid signature"},
		{"tampered", append(bytes.Clone(good[:len(good)-1]), good[len(good)-1]^1), "invalid signature"},
		{"unknown version", resign(good, 2), "unknown envelope version"},
		{"malformed", resign(append(bytes.Clone(good[:33]), 0xff, 0xff), 1), "malformed envelope"},
		{"unknown target", seal(t, unknownTarget, key), "unknown target"},
		{"unknown route", seal(t, unknownRoute, key), "unknown target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deploy, reqs := newTarget(t, nil)
			s := newSetup(t, deploy, nil)
			s.enqueue(t, tt.msg)
			if len(reqs) != 0 {
				t.Error("forwarded")
			}
			if s.q.Len() != 1 {
				t.Error("acknowledged")
			}
			if !bytes.Contains(s.logs.Bytes(), []byte(`"msg":"`+tt.outcome+`"`)) {
				t.Errorf("log %s does not have outcome %q", s.logs, tt.outcome)
			}
		})
	}
}

func TestOldKeyAccepted(t *testing.T) {
	deploy, reqs := newTarget(t, nil)
	s := newSetup(t, deploy, nil)
	s.enqueue(t, seal(t, testEnvelope(), oldKey))
	if len(reqs) != 1 || s.q.Len() != 0 {
		t.Errorf("envelope signed with the second key: %d requests, %d unacknowledged", len(reqs), s.q.Len())
	}
}

func TestAckOnlyOn2xx(t *testing.T) {
	tests := []struct {
		status int
		acked  bool
	}{
		{200, true}, {202, true}, {204, true}, {299, true},
		{304, false}, {400, false}, {404, false}, {500, false}, {503, false},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			deploy, reqs := newTarget(t, status(tt.status))
			s := newSetup(t, deploy, nil)
			s.enqueue(t, seal(t, testEnvelope(), key))
			if len(reqs) != 1 {
				t.Fatalf("target got %d requests", len(reqs))
			}
			if acked := s.q.Len() == 0; acked != tt.acked {
				t.Errorf("acknowledged = %v, want %v", acked, tt.acked)
			}
		})
	}
}

func TestNoRedirects(t *testing.T) {
	elsewhere, elsewhereReqs := newTarget(t, nil)
	deploy, reqs := newTarget(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.String(), http.StatusTemporaryRedirect)
	})
	s := newSetup(t, deploy, nil)
	s.enqueue(t, seal(t, testEnvelope(), key))
	if len(reqs) != 1 || len(elsewhereReqs) != 0 {
		t.Errorf("redirect followed: %d, %d requests", len(reqs), len(elsewhereReqs))
	}
	if s.q.Len() != 1 {
		t.Error("redirect acknowledged")
	}
}

func TestTimeout(t *testing.T) {
	release := make(chan struct{})
	deploy, _ := newTarget(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	s := newSetup(t, deploy, func(o *Options) { o.Timeout = 100 * time.Millisecond })
	start := time.Now()
	s.enqueue(t, seal(t, testEnvelope(), key))
	if time.Since(start) > 2*time.Second {
		t.Errorf("forward took %v", time.Since(start))
	}
	if s.q.Len() != 1 {
		t.Error("acknowledged after a timeout")
	}
	if !bytes.Contains(s.logs.Bytes(), []byte(`"msg":"target timeout"`)) {
		t.Errorf("log: %s", s.logs)
	}
}

func TestUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u, _ := url.Parse(srv.URL)
	srv.Close()
	s := newSetup(t, u, nil)
	s.enqueue(t, seal(t, testEnvelope(), key))
	if s.q.Len() != 1 {
		t.Error("acknowledged")
	}
	if !bytes.Contains(s.logs.Bytes(), []byte(`"msg":"target unreachable"`)) {
		t.Errorf("log: %s", s.logs)
	}
}

func TestDedupe(t *testing.T) {
	deploy, reqs := newTarget(t, nil)
	notify, notifyReqs := newTarget(t, nil)
	s := newSetup(t, deploy, func(o *Options) {
		o.Targets["github-infra"]["notify"] = notify
		o.DedupeWindow = time.Minute
	})
	now := time.Now()
	s.f.now = func() time.Time { return now }

	e := testEnvelope()
	s.enqueue(t, seal(t, e, key))
	s.enqueue(t, seal(t, e, key)) // the sender retried
	if len(reqs) != 1 {
		t.Errorf("duplicate forwarded: %d requests", len(reqs))
	}
	if s.q.Len() != 0 {
		t.Error("duplicate not acknowledged")
	}
	if !bytes.Contains(s.logs.Bytes(), []byte(`"msg":"duplicate"`)) {
		t.Errorf("log: %s", s.logs)
	}

	// Another target or delivery ID is not a duplicate.
	other := testEnvelope()
	other.Target = "notify"
	s.enqueue(t, seal(t, other, key))
	other = testEnvelope()
	other.DeliveryID = "another"
	s.enqueue(t, seal(t, other, key))
	if len(notifyReqs) != 1 || len(reqs) != 2 {
		t.Errorf("requests: deploy %d, notify %d", len(reqs), len(notifyReqs))
	}

	// After the window, the same delivery is forwarded again.
	now = now.Add(time.Minute)
	s.enqueue(t, seal(t, e, key))
	if len(reqs) != 3 {
		t.Errorf("not forwarded after the dedupe window: %d requests", len(reqs))
	}
}

// A failed delivery is not remembered, so its redelivery is forwarded.
func TestDedupeOnlyAfterSuccess(t *testing.T) {
	var code atomic.Int32
	code.Store(http.StatusInternalServerError)
	deploy, reqs := newTarget(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(code.Load())) })
	s := newSetup(t, deploy, nil)
	e := testEnvelope()
	s.enqueue(t, seal(t, e, key))
	code.Store(http.StatusOK)
	s.enqueue(t, seal(t, e, key))
	if len(reqs) != 2 {
		t.Errorf("target got %d requests, want 2", len(reqs))
	}
}

func TestDedupeBounded(t *testing.T) {
	d := newDedupe(time.Hour, 2)
	now := time.Now()
	a, b, c := dedupeKey{deliveryID: "a"}, dedupeKey{deliveryID: "b"}, dedupeKey{deliveryID: "c"}
	d.add(a, now)
	d.add(b, now)
	d.add(c, now)
	if d.seen(a, now) || !d.seen(b, now) || !d.seen(c, now) {
		t.Error("oldest key not dropped")
	}
	if len(d.at) != 2 {
		t.Errorf("%d keys stored", len(d.at))
	}
	// A key added again after expiring stays until its new expiry.
	d2 := newDedupe(time.Minute, 10)
	d2.add(a, now)
	d2.add(a, now.Add(2*time.Minute))
	if !d2.seen(a, now.Add(2*time.Minute+30*time.Second)) {
		t.Error("re-added key dropped by its old entry")
	}
}

func TestNewRejects(t *testing.T) {
	u, _ := url.Parse("http://deploy.example/hooks")
	withQuery, _ := url.Parse("http://deploy.example/hooks?a=1")
	q := queuefake.New(queuefake.Options{AckDeadline: 30 * time.Second})
	tests := []struct {
		name   string
		change func(*Options)
	}{
		{"no queue", func(o *Options) { o.Queue = nil }},
		{"no keys", func(o *Options) { o.Keys = nil }},
		{"no timeout", func(o *Options) { o.Timeout = 0 }},
		{"no dedupe window", func(o *Options) { o.DedupeWindow = 0 }},
		{"timeout not below ack deadline", func(o *Options) { o.Timeout = 30 * time.Second }},
		{"target URL with query", func(o *Options) { o.Targets["r"]["t"] = withQuery }},
		{"no target URL", func(o *Options) { o.Targets["r"]["t"] = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := Options{
				Queue:        q,
				Keys:         [][]byte{key},
				Targets:      map[string]map[string]*url.URL{"r": {"t": u}},
				Timeout:      time.Second,
				DedupeWindow: time.Minute,
			}
			tt.change(&opts)
			_, err := New(opts)
			if err == nil {
				t.Fatal("no error")
			}
			if bytes.Contains([]byte(err.Error()), []byte("deploy.example")) {
				t.Errorf("error %q contains the target URL", err)
			}
		})
	}
}

func TestRun(t *testing.T) {
	deploy, reqs := newTarget(t, nil)
	s := newSetup(t, deploy, nil)
	for _, id := range []string{"one", "two"} {
		e := testEnvelope()
		e.DeliveryID = id
		s.q.Send(t.Context(), seal(t, e, key))
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- s.f.Run(ctx) }()
	for range 2 {
		select {
		case <-reqs:
		case <-time.After(5 * time.Second):
			t.Fatal("message not forwarded")
		}
	}
	waitFor(t, func() bool { return s.q.Len() == 0 })
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v", err)
	}
}

// On shutdown, the forwarder finishes the forward in progress.
func TestRunFinishesCurrentForward(t *testing.T) {
	release := make(chan struct{})
	deploy, reqs := newTarget(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	})
	s := newSetup(t, deploy, nil)
	s.q.Send(t.Context(), seal(t, testEnvelope(), key))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- s.f.Run(ctx) }()

	select {
	case <-reqs:
	case <-time.After(5 * time.Second):
		t.Fatal("message not forwarded")
	}
	cancel()
	select {
	case <-done:
		t.Fatal("Run returned during a forward")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if s.q.Len() != 0 {
		t.Error("finished forward not acknowledged")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
