package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NiekKeijzer/webhook-relay/config"
	"github.com/NiekKeijzer/webhook-relay/internal/queuefake"
	"github.com/NiekKeijzer/webhook-relay/queue"
)

const (
	e2eRouteID  = "16fd2706-8baf-433b-82eb-8c7fada847da"
	e2eSecret   = "e2e route secret"
	e2eDelivery = "72d3162e-cc78-11e3-81ab-4c9367dc0958"
)

const e2eConfig = `
version = 1

[[routes]]
name = "github-infra"
ids = ["%s"]
verifier = { type = "github-hmac-sha256", secrets_env = ["ROUTE_GITHUB_INFRA_SECRET"] }
delivery_id_header = "X-GitHub-Delivery"
headers = ["X-GitHub-Event", "X-GitHub-Delivery"]
targets = [
  { name = "deploy", url = "%s/hooks/github" },
  { name = "notify", url = "%s/github" },
]
`

// e2eQueueType lets the test's configuration pass validation. Its fake queue
// is passed to the components directly; no configuration can open it.
var e2eQueueType = queue.Type{
	Name:     "e2e",
	Validate: func(map[string]string) (time.Duration, error) { return 30 * time.Second, nil },
}

type hit struct {
	method, path, query string
	header              http.Header
	body                []byte
}

func e2eTarget(t *testing.T) (*httptest.Server, chan hit) {
	t.Helper()
	hits := make(chan hit, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hits <- hit{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone(), body: body}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

func githubSignature(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// TestEndToEnd runs receiver → fake queue → forwarder → test targets in
// process, wired as cmd/relay wires them.
func TestEndToEnd(t *testing.T) {
	deploy, deployHits := e2eTarget(t)
	notify, notifyHits := e2eTarget(t)
	// The route secret comes from a file, with the trailing newline that
	// tools such as echo add.
	secretFile := filepath.Join(t.TempDir(), "github-infra-secret")
	if err := os.WriteFile(secretFile, []byte(e2eSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := lookup(map[string]string{
		configEnv:                               fmt.Sprintf(e2eConfig, e2eRouteID, deploy.URL, notify.URL),
		"RELAY_QUEUE_TYPE":                      "e2e",
		"RELAY_ENVELOPE_KEY":                    strings.Repeat("e", 32),
		"RELAY_FORWARDER_TIMEOUT_SECONDS":       "5",
		"RELAY_FORWARDER_DEDUPE_WINDOW_SECONDS": "3600",
		"ROUTE_GITHUB_INFRA_SECRET_FILE":        secretFile,
	})

	regs, err := newRegistries([]queue.Type{e2eQueueType})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig("", env, regs)
	if err != nil {
		t.Fatal(err)
	}
	renv, err := cfg.ResolveReceiver(env, regs)
	if err != nil {
		t.Fatal(err)
	}
	fenv, err := config.ResolveForwarder(env, regs)
	if err != nil {
		t.Fatal(err)
	}
	q := queuefake.New(queuefake.Options{AckDeadline: 30 * time.Second})
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h, err := newReceiver(cfg, regs, renv, q, logger)
	if err != nil {
		t.Fatal(err)
	}
	f, err := newForwarder(cfg, fenv, q, logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- f.Run(ctx) }()
	receiver := httptest.NewServer(h)
	defer receiver.Close()

	post := func(body, signature string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", receiver.URL+"/"+e2eRouteID+"?source=e2e", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "GitHub-Hookshot/e2e")
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-GitHub-Delivery", e2eDelivery)
		req.Header.Set("X-Hub-Signature-256", signature)
		req.Header.Set("X-Not-Kept", "dropped")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		reply, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(reply)
	}

	t.Run("signed request fans out", func(t *testing.T) {
		body := `{"zen":"Speak like a human.","hook_id":1}`
		sig := githubSignature(e2eSecret, body)
		if status, _ := post(body, sig); status != http.StatusAccepted {
			t.Fatalf("receiver answered %d", status)
		}
		for name, hits := range map[string]chan hit{"/hooks/github": deployHits, "/github": notifyHits} {
			var got hit
			select {
			case got = <-hits:
			case <-time.After(5 * time.Second):
				t.Fatalf("target %s got nothing", name)
			}
			if got.method != "POST" || got.path != name || got.query != "source=e2e" || string(got.body) != body {
				t.Errorf("%s got %s %s?%s %q", name, got.method, got.path, got.query, got.body)
			}
			// The target can check GitHub's signature itself.
			if got.header.Get("X-Hub-Signature-256") != sig || githubSignature(e2eSecret, string(got.body)) != sig {
				t.Errorf("%s: signature does not verify", name)
			}
			for header, want := range map[string]string{
				"Content-Type":        "application/json",
				"User-Agent":          "GitHub-Hookshot/e2e",
				"X-Github-Event":      "push",
				"X-Github-Delivery":   e2eDelivery,
				"X-Relay-Delivery-Id": e2eDelivery,
				"X-Relay-Attempt":     "1",
			} {
				if v := got.header.Get(header); v != want {
					t.Errorf("%s: %s = %q, want %q", name, header, v, want)
				}
			}
			if _, err := time.Parse(time.RFC3339, got.header.Get("X-Relay-Received-At")); err != nil {
				t.Errorf("%s: X-Relay-Received-At: %v", name, err)
			}
			if got.header.Get("X-Not-Kept") != "" {
				t.Errorf("%s got a header that is not kept", name)
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for q.Len() != 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if q.Len() != 0 || q.Sent() != 2 {
			t.Errorf("queue: %d unacknowledged of %d sent", q.Len(), q.Sent())
		}
	})

	t.Run("bad signature is rejected", func(t *testing.T) {
		sent := q.Sent()
		body := `{"zen":"Forged."}`
		status, reply := post(body, githubSignature("wrong secret", body))
		if status != http.StatusUnauthorized || reply != "" {
			t.Errorf("receiver answered %d %q", status, reply)
		}
		if q.Sent() != sent {
			t.Error("forged request enqueued")
		}
		time.Sleep(200 * time.Millisecond)
		if len(deployHits)+len(notifyHits) != 0 {
			t.Error("forged request reached a target")
		}
	})

	cancel()
	if err := <-done; err != nil {
		t.Errorf("forwarder: %v", err)
	}
}
