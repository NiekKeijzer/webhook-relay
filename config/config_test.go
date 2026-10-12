package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/NiekKeijzer/webhook-relay/plugin"
)

const base = `
version = 1

[[routes]]
name = "github-infra"
ids = ["7c9e6679-7425-40de-944b-e07fc1f90ae7"]
methods = ["POST"]
verifier = { type = "hmac", params = {}, secrets_env = ["ROUTE_GITHUB_INFRA_SECRET"] }
acceptor = { type = "default", params = { status = 202 } }
delivery_id_header = "X-GitHub-Delivery"
headers = ["X-GitHub-Event", "x-github-delivery"]
targets = [
  { name = "deploy", url = "http://deploy.internal.example/hooks/github" },
  { name = "notify", url = "https://notify.internal.example:8443/github" },
]

[[routes]]
name = "open"
ids = ["0f8fad5b-d9cb-469f-a165-70867728950e"]
verifier = { type = "none" }
targets = [{ name = "sink", url = "http://sink.internal.example/" }]
`

// fakePlugins stands in for the registries in cmd/relay.
type fakePlugins struct{}

func (fakePlugins) Verifier(typ string, p plugin.Params) (bool, error) {
	switch typ {
	case "hmac":
		return true, p.Decode(&struct{}{})
	case "none":
		return false, p.Decode(&struct{}{})
	case "with-extra":
		return false, p.Decode(&struct {
			Extra int `toml:"extra"`
		}{})
	}
	return false, fmt.Errorf("unknown verifier type %q", typ)
}

func (fakePlugins) Acceptor(typ string, p plugin.Params) error {
	switch typ {
	case "default":
		return p.Decode(&struct {
			Status *int `toml:"status"`
		}{})
	case "lazy": // ignores its params
		return nil
	}
	return fmt.Errorf("unknown acceptor type %q", typ)
}

func TestParseValid(t *testing.T) {
	cfg, err := Parse([]byte(base), fakePlugins{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routes) != 2 {
		t.Fatalf("got %d routes", len(cfg.Routes))
	}

	gh := cfg.Routes[0]
	if gh.Name != "github-infra" || !slices.Equal(gh.IDs, []string{"7c9e6679-7425-40de-944b-e07fc1f90ae7"}) {
		t.Errorf("route = %+v", gh)
	}
	if gh.Verifier.Type != "hmac" || !slices.Equal(gh.Verifier.SecretsEnv, []string{"ROUTE_GITHUB_INFRA_SECRET"}) {
		t.Errorf("verifier = %+v", gh.Verifier)
	}
	var ap struct {
		Status int `toml:"status"`
	}
	if err := gh.Acceptor.Params.Decode(&ap); err != nil || gh.Acceptor.Type != "default" || ap.Status != 202 {
		t.Errorf("acceptor = %+v, status %d, err %v", gh.Acceptor, ap.Status, err)
	}
	if gh.DeliveryIDHeader != "X-Github-Delivery" {
		t.Errorf("delivery ID header = %q, want canonical form", gh.DeliveryIDHeader)
	}
	if !slices.Equal(gh.Headers, []string{"X-Github-Event", "X-Github-Delivery"}) {
		t.Errorf("headers = %v, want canonical forms", gh.Headers)
	}
	if len(gh.Targets) != 2 || gh.Targets[0].Name != "deploy" || gh.Targets[0].URL.String() != "http://deploy.internal.example/hooks/github" ||
		gh.Targets[1].Name != "notify" || gh.Targets[1].URL.String() != "https://notify.internal.example:8443/github" {
		t.Errorf("targets = %+v", gh.Targets)
	}

	open := cfg.Routes[1]
	if !slices.Equal(open.Methods, []string{"POST"}) {
		t.Errorf("default methods = %v", open.Methods)
	}
	if open.Acceptor.Type != DefaultAcceptor {
		t.Errorf("default acceptor = %q", open.Acceptor.Type)
	}
	if open.DeliveryIDHeader != "" || open.Headers != nil {
		t.Errorf("route = %+v", open)
	}
}

// sensitive lists values that no error may contain: route IDs, target URLs
// and the bad values the cases below put in their place.
var sensitive = []string{"7c9e6679", "0f8fad5b", "internal.example", "sensitive"}

func TestParseErrors(t *testing.T) {
	const ghVerifier = `verifier = { type = "hmac", params = {}, secrets_env = ["ROUTE_GITHUB_INFRA_SECRET"] }`
	const openTargets = `targets = [{ name = "sink", url = "http://sink.internal.example/" }]`
	const ghID = `ids = ["7c9e6679-7425-40de-944b-e07fc1f90ae7"]`
	tests := []struct {
		name     string
		old, new string // replaced in base; empty old prepends new
		want     string
	}{
		// Unknown keys, in the file and in plugin params.
		{"unknown top-level key", "", "extra = 1\n", `unknown key "extra"`},
		{"former queue section", "version = 1", "version = 1\nqueue = { type = \"test\" }", `unknown key "queue"`},
		{"unknown route key", `name = "open"`, "name = \"open\"\ncolour = \"red\"", `unknown key "routes.colour"`},
		{"unknown target key", openTargets, `targets = [{ name = "sink", url = "http://sink.internal.example/", weight = 1 }]`, `unknown key "routes.targets.weight"`},
		{"unknown verifier key", ghVerifier, `verifier = { type = "hmac", params = {}, secrets_env = ["X"], secret = "x" }`, `unknown key "routes.verifier.secret"`},
		{"key in wrong case", `name = "open"`, `Name = "open"`, `unknown key "routes.Name"`},
		{"unknown params key", `verifier = { type = "none" }`, `verifier = { type = "none", params = { foo = 1 } }`, `route "open": verifier: params: unknown keys ["foo"]`},
		{"params key in wrong case", `params = { status = 202 }`, `params = { Status = 202 }`, `unknown keys ["Status"]`},
		{"mistyped param", `params = { status = 202 }`, `params = { status = "202" }`, `route "github-infra": acceptor: params: (last key "status"): incompatible types`},
		{"plugin ignores params", `acceptor = { type = "default", params = { status = 202 } }`, `acceptor = { type = "lazy", params = { x = 1 } }`, `acceptor: unknown params keys ["x"]`},

		// Targets written as blocks.
		{"targets block", openTargets, "\n[[routes.targets]]\nname = \"sink\"\nurl = \"http://sink.internal.example/\"\n", `route "open": targets must be an inline array`},

		// Required fields.
		{"missing version", "version = 1", "", "version is required"},
		{"wrong version", "version = 1", "version = 2", "version must be 1"},
		{"missing verifier", ghVerifier, "", `route "github-infra": verifier is required`},
		{"missing verifier type", `verifier = { type = "none" }`, `verifier = { params = {} }`, `route "open": verifier.type is required`},
		{"missing ids", ghID, "", `route "github-infra": ids needs at least one entry`},
		{"missing targets", openTargets, "", `route "open": targets needs at least one entry`},

		// Names.
		{"bad route name", `name = "open"`, `name = "Open"`, "routes[1]: name must match"},
		{"duplicate route name", `name = "open"`, `name = "github-infra"`, `route "github-infra": name is used by more than one route`},
		{"bad target name", `name = "sink"`, `name = "Sink"`, `route "open": targets[0]: name must match`},
		{"duplicate target name", `name = "notify"`, `name = "deploy"`, `target "deploy": name is used by more than one target`},

		// IDs.
		{"id not a uuid", ghID, `ids = ["sensitive"]`, `ids[0] is not a lowercase version 4 UUID`},
		{"id in uppercase", ghID, `ids = ["7C9E6679-7425-40DE-944B-E07FC1F90AE7"]`, `ids[0] is not a lowercase version 4 UUID`},
		{"id version 1", ghID, `ids = ["7c9e6679-7425-10de-944b-e07fc1f90ae7"]`, `ids[0] is not a lowercase version 4 UUID`},
		{"id wrong variant", ghID, `ids = ["7c9e6679-7425-40de-c44b-e07fc1f90ae7"]`, `ids[0] is not a lowercase version 4 UUID`},
		{"id on two routes", `ids = ["0f8fad5b-d9cb-469f-a165-70867728950e"]`, ghID, `route "open": ids[0] is also used by route "github-infra"`},

		// Methods.
		{"empty methods", `methods = ["POST"]`, "methods = []", "methods needs at least one entry"},
		{"lowercase method", `methods = ["POST"]`, `methods = ["post"]`, "methods[0] must be one of"},
		{"unsupported method", `methods = ["POST"]`, `methods = ["CONNECT"]`, "methods[0] must be one of"},
		{"duplicate method", `methods = ["POST"]`, `methods = ["POST", "POST"]`, "methods[1] is a duplicate"},

		// Headers.
		{"reserved header", `headers = ["X-GitHub-Event", "x-github-delivery"]`, `headers = ["x-relay-attempt"]`, "headers[0]: names starting with X-Relay- are reserved"},
		{"hop-by-hop header", `headers = ["X-GitHub-Event", "x-github-delivery"]`, `headers = ["Transfer-Encoding"]`, "headers[0]: Transfer-Encoding cannot be kept"},
		{"host header", `headers = ["X-GitHub-Event", "x-github-delivery"]`, `headers = ["X-GitHub-Event", "host"]`, "headers[1]: Host cannot be kept"},
		{"proxy header", `headers = ["X-GitHub-Event", "x-github-delivery"]`, `headers = ["Proxy-Authorization"]`, "Proxy-Authorization cannot be kept"},
		{"invalid header name", `headers = ["X-GitHub-Event", "x-github-delivery"]`, `headers = ["bad header"]`, "headers[0]: not a valid header name"},
		{"reserved delivery ID header", `delivery_id_header = "X-GitHub-Delivery"`, `delivery_id_header = "X-Relay-Delivery-Id"`, "delivery_id_header: names starting with X-Relay- are reserved"},
		{"empty delivery ID header", `delivery_id_header = "X-GitHub-Delivery"`, `delivery_id_header = ""`, "delivery_id_header: not a valid header name"},

		// Target URLs. Errors never contain the URL.
		{"relative url", "http://sink.internal.example/", "/sensitive", `target "sink": url must be an absolute http or https URL`},
		{"other scheme", "http://sink.internal.example/", "ftp://sensitive.example/", "url must be an absolute http or https URL"},
		{"no host", "http://sink.internal.example/", "http:///sensitive", "url must be an absolute http or https URL"},
		{"unparsable url", "http://sink.internal.example/", "http://sensitive.example:port/", "url must be an absolute http or https URL"},
		{"url with query", "http://sink.internal.example/", "http://sensitive.example/?a=1", "url must not have a query string"},
		{"url with empty query", "http://sink.internal.example/", "http://sensitive.example/?", "url must not have a query string"},
		{"url with fragment", "http://sink.internal.example/", "http://sensitive.example/#top", "url must not have a fragment"},
		{"url with user info", "http://sink.internal.example/", "http://user:sensitive@sensitive.example/", "url must not contain user info"},

		// Types and secrets.
		{"unknown verifier type", `verifier = { type = "none" }`, `verifier = { type = "magic" }`, `route "open": verifier: unknown verifier type "magic"`},
		{"unknown acceptor type", `acceptor = { type = "default", params = { status = 202 } }`, `acceptor = { type = "magic" }`, `route "github-infra": acceptor: unknown acceptor type "magic"`},
		{"verifier needs secrets", ghVerifier, `verifier = { type = "hmac" }`, `verifier type "hmac" needs at least one secrets_env entry`},
		{"verifier takes no secrets", `verifier = { type = "none" }`, `verifier = { type = "none", secrets_env = ["X"] }`, `verifier type "none" takes no secrets`},

		// Environment variable names.
		{"bad secret env name", `secrets_env = ["ROUTE_GITHUB_INFRA_SECRET"]`, `secrets_env = ["A-B"]`, "verifier.secrets_env[0]: not a valid environment variable name"},
		{"duplicate secret env name", `secrets_env = ["ROUTE_GITHUB_INFRA_SECRET"]`, `secrets_env = ["ROUTE_GITHUB_INFRA_SECRET", "ROUTE_GITHUB_INFRA_SECRET"]`, `verifier.secrets_env[1]: "ROUTE_GITHUB_INFRA_SECRET" is listed more than once`},
		{"secret env name for a file", `secrets_env = ["ROUTE_GITHUB_INFRA_SECRET"]`, `secrets_env = ["ROUTE_GITHUB_INFRA_SECRET_FILE"]`, "verifier.secrets_env[0]: names ending in _FILE are reserved for secret files"},

		// Syntax and range errors do not quote the file.
		{"syntax error", `name = "open"`, `name = "http://sensitive.example/" sensitive`, "line 18, column"},
		{"out of range", "version = 1", "version = 99999999999999999999", "invalid TOML or value out of range"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := tt.new + base
			if tt.old != "" {
				if !strings.Contains(base, tt.old) {
					t.Fatalf("base does not contain %q", tt.old)
				}
				data = strings.Replace(base, tt.old, tt.new, 1)
			}
			_, err := Parse([]byte(data), fakePlugins{})
			if err == nil {
				t.Fatalf("no error, want %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q\ndoes not contain %q", err, tt.want)
			}
			for _, s := range sensitive {
				if strings.Contains(strings.ToLower(err.Error()), s) {
					t.Errorf("error %q contains %q", err, s)
				}
			}
		})
	}
}

// A key one route's plugin knows does not become known on another route.
// MetaData.Undecoded alone would miss this, because it lists key paths for
// all routes together.
func TestParseParamsPerRoute(t *testing.T) {
	data := strings.Replace(base, `verifier = { type = "none" }`, `verifier = { type = "none", params = { extra = 1 } }`, 1)
	data = strings.Replace(data, `verifier = { type = "hmac", params = {}, secrets_env = ["ROUTE_GITHUB_INFRA_SECRET"] }`,
		`verifier = { type = "with-extra", params = { extra = 1 } }`, 1)
	_, err := Parse([]byte(data), fakePlugins{})
	want := `route "open": verifier: params: unknown keys ["extra"]`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error %v, want %q", err, want)
	}
	if strings.Contains(err.Error(), "github-infra") {
		t.Errorf("error %q blames the route whose plugin knows the key", err)
	}
}

// A [[routes.targets]] block is rejected even when a later route writes its
// targets inline, which makes MetaData.Type report "Array" for both.
func TestParseTargetsBlockBeforeInline(t *testing.T) {
	data := strings.Replace(base, `targets = [
  { name = "deploy", url = "http://deploy.internal.example/hooks/github" },
  { name = "notify", url = "https://notify.internal.example:8443/github" },
]`, `
[[routes.targets]]
name = "deploy"
url = "http://deploy.internal.example/hooks/github"
`, 1)
	_, err := Parse([]byte(data), fakePlugins{})
	want := `route "github-infra": targets must be an inline array`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error %v, want %q", err, want)
	}
}

func TestParseReportsAllErrors(t *testing.T) {
	data := strings.Replace(base, "version = 1", "version = 2", 1)
	data = strings.Replace(data, `name = "open"`, `name = "Open"`, 1)
	_, err := Parse([]byte(data), fakePlugins{})
	if err == nil || !strings.Contains(err.Error(), "version must be 1") || !strings.Contains(err.Error(), "routes[1]: name must match") {
		t.Fatalf("error %v, want both problems", err)
	}
}

func TestParseRoutesInline(t *testing.T) {
	data := `
version = 1
routes = [
  { name = "a", ids = ["0f8fad5b-d9cb-469f-a165-70867728950e"], verifier = { type = "none" }, targets = [{ name = "t", url = "http://t.example/" }] },
]
`
	cfg, err := Parse([]byte(data), fakePlugins{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routes) != 1 || cfg.Routes[0].Name != "a" {
		t.Errorf("routes = %+v", cfg.Routes)
	}
}

// fakeQueues stands in for the queue registry in cmd/relay.
type fakeQueues map[string]fakeQueue

type fakeQueue struct {
	params, credentials []string
	ackDeadline         time.Duration
}

var queues = fakeQueues{
	"test":        {params: []string{"REGION", "ZONE"}, credentials: []string{"KEY"}, ackDeadline: time.Minute},
	"bad-name":    {params: []string{"region"}},
	"no-deadline": {params: []string{"REGION"}},
}

func (f fakeQueues) QueueNames(typ string) ([]string, []string, error) {
	q, ok := f[typ]
	if !ok {
		return nil, nil, fmt.Errorf("unknown queue type %q", typ)
	}
	return q.params, q.credentials, nil
}

// ValidateQueue requires REGION.
func (f fakeQueues) ValidateQueue(typ string, params map[string]string) (time.Duration, error) {
	if params["REGION"] == "" {
		return 0, errors.New("REGION is required")
	}
	return f[typ].ackDeadline, nil
}

// env is a fake environment that records lookups.
type env struct {
	values map[string]string
	looked []string
}

func (e *env) lookup(name string) (string, bool) {
	e.looked = append(e.looked, name)
	v, ok := e.values[name]
	return v, ok
}

var (
	keyA = strings.Repeat("a", MinEnvelopeKeySize)
	keyB = strings.Repeat("b", MinEnvelopeKeySize+8)
)

func fullEnv() *env {
	return &env{values: map[string]string{
		"RELAY_QUEUE_TYPE":                      "test",
		"RELAY_QUEUE_REGION":                    "nl-ams",
		"RELAY_QUEUE_KEY":                       "queue credential",
		"RELAY_ENVELOPE_KEY":                    keyA,
		"RELAY_ENVELOPE_KEY_PREVIOUS":           keyB,
		"RELAY_FORWARDER_TIMEOUT_SECONDS":       "10",
		"RELAY_FORWARDER_DEDUPE_WINDOW_SECONDS": "3600",
		"ROUTE_GITHUB_INFRA_SECRET":             "route secret",
	}}
}

func parseBase(t *testing.T) *Config {
	t.Helper()
	cfg, err := Parse([]byte(base), fakePlugins{})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func checkQueue(t *testing.T, q Queue) {
	t.Helper()
	// ZONE is declared but unset, so it is left out.
	if q.Type != "test" || !maps.Equal(q.Params, map[string]string{"REGION": "nl-ams"}) ||
		!maps.Equal(q.Credentials, map[string]string{"KEY": "queue credential"}) {
		t.Errorf("queue = %+v", q)
	}
}

func TestResolveReceiver(t *testing.T) {
	e := fullEnv()
	got, err := parseBase(t).ResolveReceiver(e.lookup, queues)
	if err != nil {
		t.Fatal(err)
	}
	checkQueue(t, got.Queue)
	if string(got.EnvelopeKey) != keyA {
		t.Errorf("envelope key = %q", got.EnvelopeKey)
	}
	if s := got.RouteSecrets["github-infra"]; len(s) != 1 || string(s[0]) != "route secret" {
		t.Errorf("route secrets = %q", s)
	}
	if s := got.RouteSecrets["open"]; s != nil {
		t.Errorf("route without secrets got %q", s)
	}
	for _, name := range []string{"RELAY_ENVELOPE_KEY_PREVIOUS", "RELAY_FORWARDER_TIMEOUT_SECONDS", "RELAY_FORWARDER_DEDUPE_WINDOW_SECONDS"} {
		if slices.Contains(e.looked, name) {
			t.Errorf("receiver read %s", name)
		}
	}
}

func TestResolveForwarder(t *testing.T) {
	e := fullEnv()
	got, err := ResolveForwarder(e.lookup, queues)
	if err != nil {
		t.Fatal(err)
	}
	checkQueue(t, got.Queue)
	if len(got.EnvelopeKeys) != 2 || string(got.EnvelopeKeys[0]) != keyA || string(got.EnvelopeKeys[1]) != keyB {
		t.Errorf("envelope keys = %q", got.EnvelopeKeys)
	}
	if got.Timeout != 10*time.Second || got.DedupeWindow != time.Hour {
		t.Errorf("timeout %s, dedupe window %s", got.Timeout, got.DedupeWindow)
	}
	for _, name := range []string{"ROUTE_GITHUB_INFRA_SECRET", "ROUTE_GITHUB_INFRA_SECRET_FILE"} {
		if slices.Contains(e.looked, name) {
			t.Errorf("forwarder read %s", name)
		}
	}

	// Outside a rotation, there is one envelope key.
	e = fullEnv()
	delete(e.values, "RELAY_ENVELOPE_KEY_PREVIOUS")
	got, err = ResolveForwarder(e.lookup, queues)
	if err != nil || len(got.EnvelopeKeys) != 1 {
		t.Errorf("without a previous key: %v, %d keys", err, len(got.EnvelopeKeys))
	}
}

// Every secret can come from the file named by NAME_FILE. Trailing line
// breaks are removed; all other bytes are kept.
func TestResolveSecretFiles(t *testing.T) {
	dir := t.TempDir()
	e := fullEnv()
	for name, content := range map[string]string{
		"ROUTE_GITHUB_INFRA_SECRET":   "route secret\n",
		"RELAY_ENVELOPE_KEY":          keyA + "\r\n",
		"RELAY_ENVELOPE_KEY_PREVIOUS": keyB,
		"RELAY_QUEUE_KEY":             "line one\nline two\n\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		delete(e.values, name)
		e.values[name+"_FILE"] = path
	}

	renv, err := parseBase(t).ResolveReceiver(e.lookup, queues)
	if err != nil {
		t.Fatal(err)
	}
	if s := renv.RouteSecrets["github-infra"]; len(s) != 1 || string(s[0]) != "route secret" {
		t.Errorf("route secrets = %q", s)
	}
	if string(renv.EnvelopeKey) != keyA {
		t.Errorf("envelope key = %q", renv.EnvelopeKey)
	}
	if c := renv.Queue.Credentials["KEY"]; c != "line one\nline two" {
		t.Errorf("queue credential = %q", c)
	}

	fenv, err := ResolveForwarder(e.lookup, queues)
	if err != nil {
		t.Fatal(err)
	}
	if len(fenv.EnvelopeKeys) != 2 || string(fenv.EnvelopeKeys[0]) != keyA || string(fenv.EnvelopeKeys[1]) != keyB {
		t.Errorf("envelope keys = %q", fenv.EnvelopeKeys)
	}
}

func TestResolveErrors(t *testing.T) {
	cfg := parseBase(t)
	dir := t.TempDir()
	emptyFile := filepath.Join(dir, "sensitive-empty")
	if err := os.WriteFile(emptyFile, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missingFile := filepath.Join(dir, "sensitive-missing")
	const shortKey = "short secret key value"
	receiver := func(l LookupFunc) error { _, err := cfg.ResolveReceiver(l, queues); return err }
	forwarder := func(l LookupFunc) error { _, err := ResolveForwarder(l, queues); return err }

	type vars map[string]string // an empty value removes the variable
	tests := []struct {
		name    string
		change  vars
		resolve func(LookupFunc) error
		want    string
	}{
		// Secrets.
		{"missing route secret", vars{"ROUTE_GITHUB_INFRA_SECRET": ""}, receiver, "environment variable ROUTE_GITHUB_INFRA_SECRET (or ROUTE_GITHUB_INFRA_SECRET_FILE) is missing or empty"},
		{"missing envelope key", vars{"RELAY_ENVELOPE_KEY": ""}, forwarder, "environment variable RELAY_ENVELOPE_KEY (or RELAY_ENVELOPE_KEY_FILE) is missing or empty"},
		{"short envelope key", vars{"RELAY_ENVELOPE_KEY": shortKey}, receiver, "envelope key in RELAY_ENVELOPE_KEY is shorter than 32 bytes"},
		{"short previous envelope key", vars{"RELAY_ENVELOPE_KEY_PREVIOUS": shortKey}, forwarder, "envelope key in RELAY_ENVELOPE_KEY_PREVIOUS is shorter than 32 bytes"},
		{"missing queue credential", vars{"RELAY_QUEUE_KEY": ""}, receiver, "environment variable RELAY_QUEUE_KEY (or RELAY_QUEUE_KEY_FILE) is missing or empty"},
		{"secret and file", vars{"RELAY_QUEUE_KEY_FILE": emptyFile}, forwarder, "set either RELAY_QUEUE_KEY or RELAY_QUEUE_KEY_FILE, not both"},
		{"missing file", vars{"ROUTE_GITHUB_INFRA_SECRET": "", "ROUTE_GITHUB_INFRA_SECRET_FILE": missingFile}, receiver, "cannot read the file named by ROUTE_GITHUB_INFRA_SECRET_FILE: no such file or directory"},
		{"secret in place of the path", vars{"ROUTE_GITHUB_INFRA_SECRET": "", "ROUTE_GITHUB_INFRA_SECRET_FILE": "route secret"}, receiver, "cannot read the file named by ROUTE_GITHUB_INFRA_SECRET_FILE"},
		{"empty file", vars{"RELAY_ENVELOPE_KEY": "", "RELAY_ENVELOPE_KEY_FILE": emptyFile}, receiver, "the file named by RELAY_ENVELOPE_KEY_FILE is empty"},

		// Queue.
		{"missing queue type", vars{"RELAY_QUEUE_TYPE": ""}, forwarder, "environment variable RELAY_QUEUE_TYPE is missing or empty"},
		{"unknown queue type", vars{"RELAY_QUEUE_TYPE": "example-backend"}, receiver, `queue: unknown queue type "example-backend"`},
		{"invalid queue params", vars{"RELAY_QUEUE_REGION": ""}, forwarder, "queue: REGION is required"},
		{"bad queue setting name", vars{"RELAY_QUEUE_TYPE": "bad-name"}, receiver, `queue: type "bad-name": invalid setting name "region"`},
		{"no ack deadline", vars{"RELAY_QUEUE_TYPE": "no-deadline"}, forwarder, `queue: type "no-deadline" reports no ack deadline`},

		// Forwarder settings.
		{"missing timeout", vars{"RELAY_FORWARDER_TIMEOUT_SECONDS": ""}, forwarder, "environment variable RELAY_FORWARDER_TIMEOUT_SECONDS is missing or empty"},
		{"timeout not a number", vars{"RELAY_FORWARDER_TIMEOUT_SECONDS": "10s"}, forwarder, "RELAY_FORWARDER_TIMEOUT_SECONDS must be a whole number of seconds, at least 1"},
		{"zero dedupe window", vars{"RELAY_FORWARDER_DEDUPE_WINDOW_SECONDS": "0"}, forwarder, "RELAY_FORWARDER_DEDUPE_WINDOW_SECONDS must be a whole number of seconds, at least 1"},
		{"timeout too large", vars{"RELAY_FORWARDER_TIMEOUT_SECONDS": "9223372037"}, forwarder, "RELAY_FORWARDER_TIMEOUT_SECONDS is too large"},
		{"timeout out of range", vars{"RELAY_FORWARDER_TIMEOUT_SECONDS": "99999999999999999999"}, forwarder, "RELAY_FORWARDER_TIMEOUT_SECONDS is too large"},
		{"timeout not below ack deadline", vars{"RELAY_FORWARDER_TIMEOUT_SECONDS": "60"}, forwarder, "RELAY_FORWARDER_TIMEOUT_SECONDS must be shorter than the queue's ack deadline (1m0s)"},
	}
	// Secret values and file paths, which may hold a misplaced secret.
	secrets := []string{keyA, keyB, "route secret", "queue credential", shortKey, "sensitive"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := fullEnv()
			for name, v := range tt.change {
				if v == "" {
					delete(e.values, name)
				} else {
					e.values[name] = v
				}
			}
			err := tt.resolve(e.lookup)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %v, want %q", err, tt.want)
			}
			for _, s := range secrets {
				if strings.Contains(err.Error(), s) {
					t.Errorf("error %q contains %q", err, s)
				}
			}
		})
	}
}

func TestParamsDecode(t *testing.T) {
	p := NewParams(map[string]any{"status": int64(204), "nested": map[string]any{"a": "b"}})
	var got struct {
		Status int `toml:"status"`
		Nested struct {
			A string `toml:"a"`
		} `toml:"nested"`
	}
	if err := p.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Status != 204 || got.Nested.A != "b" {
		t.Errorf("got %+v", got)
	}

	var partial struct {
		Status int `toml:"status"`
	}
	err := p.Decode(&partial)
	if err == nil || !strings.Contains(err.Error(), `unknown keys ["nested" "nested.a"]`) {
		t.Errorf("error %v, want unknown nested keys", err)
	}

	if err := NewParams(nil).Decode(&partial); err != nil {
		t.Errorf("empty params: %v", err)
	}

	var small struct {
		Status int8 `toml:"status"`
	}
	err = NewParams(map[string]any{"status": int64(300000)}).Decode(&small)
	if err == nil || strings.Contains(err.Error(), "300000") {
		t.Fatalf("error %v, want an out-of-range error without the value", err)
	}
	if !strings.Contains(err.Error(), `key "status"`) {
		t.Errorf("error %v does not name the key", err)
	}
}
