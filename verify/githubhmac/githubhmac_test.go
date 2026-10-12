package githubhmac

import (
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/NiekKeijzer/webhook-relay/config"
	"github.com/NiekKeijzer/webhook-relay/plugin"
	"github.com/NiekKeijzer/webhook-relay/verify"
)

// The example values from GitHub's "Validating webhook deliveries":
// https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries
const (
	exampleSecret    = "It's a Secret to Everybody"
	examplePayload   = "Hello, World!"
	exampleSignature = "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"
)

func newTestVerifier(t *testing.T, secrets ...string) verify.Verifier {
	t.Helper()
	var bs [][]byte
	for _, s := range secrets {
		bs = append(bs, []byte(s))
	}
	v, err := Type.New(config.NewParams(nil), bs)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func request(body string, signatures ...string) *plugin.Request {
	h := http.Header{}
	for _, s := range signatures {
		h.Add(Header, s)
	}
	return &plugin.Request{Method: "POST", Header: h, Body: []byte(body)}
}

func TestGitHubExample(t *testing.T) {
	v := newTestVerifier(t, exampleSecret)
	if err := v.Verify(request(examplePayload, exampleSignature)); err != nil {
		t.Errorf("example values rejected: %v", err)
	}
}

func TestVerify(t *testing.T) {
	tests := []struct {
		name    string
		secrets []string
		req     *plugin.Request
		ok      bool
	}{
		{"uppercase hex", []string{exampleSecret}, request(examplePayload, "sha256=757107EA0EB2509FC211221CCE984B8A37570B6D7586C22C46F4379C8B043E17"), true},
		{"second secret matches", []string{"old secret", exampleSecret}, request(examplePayload, exampleSignature), true},
		{"wrong secret", []string{"another secret"}, request(examplePayload, exampleSignature), false},
		{"changed body", []string{exampleSecret}, request(examplePayload+" ", exampleSignature), false},
		{"missing header", []string{exampleSecret}, request(examplePayload), false},
		{"two headers", []string{exampleSecret}, request(examplePayload, exampleSignature, exampleSignature), false},
		{"no prefix", []string{exampleSecret}, request(examplePayload, exampleSignature[len("sha256="):]), false},
		{"sha1 prefix", []string{exampleSecret}, request(examplePayload, "sha1="+exampleSignature[len("sha256="):]), false},
		{"short signature", []string{exampleSecret}, request(examplePayload, exampleSignature[:len(exampleSignature)-2]), false},
		{"not hex", []string{exampleSecret}, request(examplePayload, exampleSignature[:len(exampleSignature)-2]+"zz"), false},
		{"empty header", []string{exampleSecret}, request(examplePayload, ""), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := newTestVerifier(t, tt.secrets...).Verify(tt.req)
			if tt.ok && err != nil {
				t.Errorf("rejected: %v", err)
			}
			if !tt.ok && !errors.Is(err, ErrSignature) {
				t.Errorf("error %v, want ErrSignature", err)
			}
		})
	}
}

func TestHeaders(t *testing.T) {
	if got := newTestVerifier(t, exampleSecret).Headers(); !slices.Equal(got, []string{"X-Hub-Signature-256"}) {
		t.Errorf("Headers() = %v", got)
	}
}

func TestType(t *testing.T) {
	if Type.Name != "github-hmac-sha256" || !Type.NeedsSecrets {
		t.Errorf("type = %+v", Type)
	}
	if err := Type.Validate(config.NewParams(nil)); err != nil {
		t.Errorf("empty params: %v", err)
	}
	if err := Type.Validate(config.NewParams(map[string]any{"algorithm": "sha1"})); err == nil {
		t.Error("unknown param accepted")
	}
	if _, err := Type.New(config.NewParams(nil), nil); err == nil {
		t.Error("no secrets accepted")
	}
	if _, err := Type.New(config.NewParams(nil), [][]byte{{}}); err == nil {
		t.Error("empty secret accepted")
	}
}
