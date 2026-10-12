package main

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exampleConfig is the first route of the example in §10.2.
const exampleConfig = `
version = 1

[[routes]]
name = "github-infra"
ids = ["7c9e6679-7425-40de-944b-e07fc1f90ae7"]
methods = ["POST"]
verifier = { type = "github-hmac-sha256", params = {}, secrets_env = ["ROUTE_GITHUB_INFRA_SECRET"] }
acceptor = { type = "default", params = { status = 202 } }
delivery_id_header = "X-GitHub-Delivery"
headers = ["X-GitHub-Event", "X-GitHub-Delivery", "X-GitHub-Hook-ID"]
targets = [
  { name = "deploy", url = "http://deploy.internal.example/hooks/github" },
  { name = "notify", url = "http://notify.internal.example/github" },
]
`

// exampleEnv completes exampleConfig. Its queue type does not exist.
var exampleEnv = map[string]string{
	"RELAY_QUEUE_TYPE":                      "example-backend",
	"RELAY_ENVELOPE_KEY":                    strings.Repeat("k", 32),
	"RELAY_FORWARDER_TIMEOUT_SECONDS":       "10",
	"RELAY_FORWARDER_DEDUPE_WINDOW_SECONDS": "3600",
	"ROUTE_GITHUB_INFRA_SECRET":             "route secret",
}

// with returns env with the given variable added.
func with(env map[string]string, name, value string) map[string]string {
	env = maps.Clone(env)
	env[name] = value
	return env
}

func lookup(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStopsAtUnknownQueueType(t *testing.T) {
	path := writeConfig(t, exampleConfig)
	for _, args := range [][]string{
		{"receiver", "-config", path},
		{"forwarder", "-config", path},
	} {
		t.Run(args[0], func(t *testing.T) {
			var stderr bytes.Buffer
			code := run(t.Context(), args, lookup(exampleEnv), &stderr)
			if code != 1 || !strings.Contains(stderr.String(), `unknown queue type "example-backend"`) {
				t.Errorf("exit %d, stderr %q", code, stderr.String())
			}
			for _, s := range []string{"7c9e6679", "internal.example", "route secret", exampleEnv["RELAY_ENVELOPE_KEY"]} {
				if strings.Contains(stderr.String(), s) {
					t.Errorf("stderr contains %q", s)
				}
			}
		})
	}
}

func TestConfigFromEnvironment(t *testing.T) {
	var stderr bytes.Buffer
	code := run(t.Context(), []string{"forwarder"}, lookup(with(exampleEnv, configEnv, exampleConfig)), &stderr)
	if code != 1 || !strings.Contains(stderr.String(), `unknown queue type "example-backend"`) {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestInvalidConfig(t *testing.T) {
	path := writeConfig(t, strings.Replace(exampleConfig, "version = 1", "version = 2", 1))
	var stderr bytes.Buffer
	code := run(t.Context(), []string{"receiver", "-config", path}, lookup(nil), &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "version must be 1") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestConfigSource(t *testing.T) {
	path := writeConfig(t, exampleConfig)
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"both", []string{"receiver", "-config", path}, map[string]string{configEnv: exampleConfig}, "set either -config or RELAY_CONFIG, not both"},
		{"neither", []string{"receiver"}, nil, "no configuration: set -config or RELAY_CONFIG"},
		{"empty variable", []string{"forwarder"}, map[string]string{configEnv: ""}, "no configuration"},
		{"missing file", []string{"forwarder", "-config", path + ".missing"}, nil, "no such file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := run(t.Context(), tt.args, lookup(tt.env), &stderr)
			if code != 1 || !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("exit %d, stderr %q, want %q", code, stderr.String(), tt.want)
			}
		})
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"relay"},
		{"receiver", "extra"},
		{"forwarder", "-listen", ":8080"},
	} {
		var stderr bytes.Buffer
		if code := run(t.Context(), args, lookup(nil), &stderr); code != 2 || !strings.Contains(stderr.String(), "usage:") {
			t.Errorf("%q: exit %d, stderr %q", args, code, stderr.String())
		}
	}
}
