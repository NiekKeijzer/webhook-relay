package receiver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/NiekKeijzer/webhook-relay/internal/queuefake"
)

// TestLogsCarryNoPayload drives requests with marked values through every
// outcome and checks that no marked value reaches the log (invariant 7).
// Every value in a request carries "sentinel", except the delivery ID of
// verified requests, which is logged by design.
func TestLogsCarryNoPayload(t *testing.T) {
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	marked := func(method, path string) *http.Request {
		r := signed(method, path+"?sentinel_key=sentinel_value", `{"secret":"sentinel-body"}`)
		r.Header.Set("Content-Type", "application/sentinel")
		r.Header.Set("User-Agent", "sentinel-agent")
		r.Header.Set("X-Event", "sentinel-event")
		r.Header.Set("Authorization", "Bearer sentinel-token")
		r.Header.Set("X-Delivery", "verified-delivery")
		return r
	}
	scenarios := []struct {
		name   string
		change func(*Options)
		qopts  queuefake.Options
		hook   func(context.Context, int) error
		req    func() *http.Request
	}{
		{name: "enqueued", req: func() *http.Request { return marked("POST", "/"+routeID) }},
		{name: "unknown route", req: func() *http.Request { return marked("POST", "/sentinel-path") }},
		{name: "near-miss route", req: func() *http.Request { return marked("POST", "/"+routeID+"/") }},
		{name: "wrong method", req: func() *http.Request { return marked("GET", "/"+rotatedID) }},
		{name: "body too large", change: func(o *Options) { o.MaxBodyBytes = 4 }, req: func() *http.Request { return marked("POST", "/"+routeID) }},
		{name: "verification failed", req: func() *http.Request {
			r := marked("POST", "/"+routeID)
			r.Header.Set(sigHeader, "sentinel-forged")
			r.Header.Set("X-Delivery", "sentinel-unverified-delivery")
			return r
		}},
		{name: "handshake", change: func(o *Options) { o.Routes[0].Acceptor = replyWith(false, 200, "sentinel-reply") }, req: func() *http.Request { return marked("POST", "/"+routeID) }},
		{name: "invalid decision", change: func(o *Options) { o.Routes[0].Acceptor = replyWith(true, 400, "sentinel-reply") }, req: func() *http.Request { return marked("POST", "/"+routeID) }},
		{name: "envelope too large", qopts: queuefake.Options{MaxMessageSize: 10}, req: func() *http.Request { return marked("POST", "/"+routeID) }},
		{name: "send failed", hook: failOn(2, errors.New("sentinel-queue-error")), req: func() *http.Request { return marked("POST", "/"+routeID) }},
	}
	for _, sc := range scenarios {
		s := newSetup(t, func(o *Options) {
			o.Logger = logger
			if sc.change != nil {
				sc.change(o)
			}
		}, sc.qopts)
		if sc.hook != nil {
			s.queue.SetSendHook(sc.hook)
		}
		s.serve(sc.req())
	}

	out := logs.String()
	for _, forbidden := range []string{"sentinel", routeID, rotatedID, string(key)} {
		if strings.Contains(strings.ToLower(out), strings.ToLower(forbidden)) {
			t.Errorf("log contains %q:\n%s", forbidden, out)
		}
	}

	var lines []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var line map[string]any
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("log line %q: %v", sc.Text(), err)
		}
		lines = append(lines, line)
	}
	if len(lines) != len(scenarios) {
		t.Fatalf("got %d log lines for %d requests", len(lines), len(scenarios))
	}
	wantKeys := []string{"body_bytes", "delivery_id", "duration", "envelopes", "level", "msg", "route", "status", "time"}
	for i, line := range lines {
		var keys []string
		for k := range line {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, wantKeys) {
			t.Errorf("%s: log fields %v, want %v", scenarios[i].name, keys, wantKeys)
		}
	}
	if lines[0]["msg"] != "enqueued" || lines[0]["route"] != "github-infra" || lines[0]["delivery_id"] != "verified-delivery" || lines[0]["envelopes"] != 2.0 {
		t.Errorf("enqueued: %v", lines[0])
	}
	if lines[1]["route"] != "unknown" || lines[1]["status"] != 404.0 {
		t.Errorf("unknown route: %v", lines[1])
	}
	if lines[5]["msg"] != "verification failed" || lines[5]["delivery_id"] != "" {
		t.Errorf("verification failed: %v", lines[5])
	}
	if lines[9]["status"] != 503.0 || lines[9]["envelopes"] != 1.0 {
		t.Errorf("send failed: %v", lines[9])
	}
}
