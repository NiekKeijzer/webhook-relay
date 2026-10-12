package forwarder

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/NiekKeijzer/webhook-relay/envelope"
)

// TestLogsCarryNoPayload processes messages with marked values through every
// outcome and checks that no marked value, target URL or address reaches the
// log (invariant 7). Route names, target names and delivery IDs are logged
// by design and carry no mark.
func TestLogsCarryNoPayload(t *testing.T) {
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	marked := func() *envelope.Envelope {
		e := testEnvelope()
		e.Query = "sentinel_key=sentinel_value"
		e.Body = []byte(`{"secret":"sentinel-body"}`)
		e.Header = http.Header{
			"Content-Type":  {"application/sentinel"},
			"Authorization": {"Bearer sentinel-token"},
		}
		return e
	}
	unknownTarget := marked()
	unknownTarget.Target = "missing"
	sealed := seal(t, marked(), key)

	closed := httptest.NewServer(http.NotFoundHandler())
	unreachable, _ := url.Parse(closed.URL + "/sentinel-path")
	closed.Close()

	scenarios := []struct {
		name    string
		handle  http.HandlerFunc
		target  *url.URL // instead of a test target
		timeout time.Duration
		msgs    [][]byte
	}{
		{name: "delivered", msgs: [][]byte{sealed}},
		{name: "duplicate", msgs: [][]byte{sealed, sealed}},
		{name: "invalid signature", msgs: [][]byte{seal(t, marked(), bytes.Repeat([]byte("x"), 32))}},
		{name: "unknown version", msgs: [][]byte{resign(sealed, 9)}},
		{name: "malformed", msgs: [][]byte{resign(append(bytes.Clone(sealed[:33]), 0xff), 1)}},
		{name: "unknown target", msgs: [][]byte{seal(t, unknownTarget, key)}},
		{name: "rejected", handle: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "sentinel-error-body", http.StatusInternalServerError)
		}, msgs: [][]byte{sealed}},
		{name: "redirect", handle: func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://sentinel-elsewhere.example/", http.StatusFound)
		}, msgs: [][]byte{sealed}},
		{name: "timeout", handle: func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, timeout: 50 * time.Millisecond, msgs: [][]byte{sealed}},
		{name: "unreachable", target: unreachable, msgs: [][]byte{sealed}},
	}
	lines := 0
	for _, sc := range scenarios {
		target, _ := newTarget(t, sc.handle)
		target.Path = "/sentinel-path"
		if sc.target != nil {
			target = sc.target
		}
		s := newSetup(t, target, func(o *Options) {
			o.Logger = logger
			if sc.timeout != 0 {
				o.Timeout = sc.timeout
			}
		})
		for _, msg := range sc.msgs {
			s.enqueue(t, msg)
			lines++
		}
	}

	out := logs.String()
	for _, forbidden := range []string{"sentinel", "127.0.0.1", "localhost", string(key)} {
		if strings.Contains(strings.ToLower(out), forbidden) {
			t.Errorf("log contains %q:\n%s", forbidden, out)
		}
	}

	var got []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var line map[string]any
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("log line %q: %v", sc.Text(), err)
		}
		got = append(got, line)
	}
	if len(got) != lines {
		t.Fatalf("got %d log lines for %d messages", len(got), lines)
	}
	var outcomes []string
	for _, line := range got {
		outcomes = append(outcomes, line["msg"].(string))
		for _, k := range []string{"route", "target", "delivery_id", "delivery_count", "duration"} {
			if _, ok := line[k]; !ok {
				t.Errorf("log line %v has no %s", line, k)
			}
		}
	}
	want := []string{"delivered", "delivered", "duplicate", "invalid signature", "unknown envelope version", "malformed envelope",
		"unknown target", "target rejected", "target rejected", "target timeout", "target unreachable"}
	if !slices.Equal(outcomes, want) {
		t.Errorf("outcomes %q\nwant     %q", outcomes, want)
	}
}
