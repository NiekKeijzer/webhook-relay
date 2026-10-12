// Package githubhmac implements the verifier type "github-hmac-sha256" (§6.3
// of the architecture). It computes HMAC-SHA256 over the raw body with the
// route secret and compares it, in constant time, with the hex value of the
// X-Hub-Signature-256 header after its "sha256=" prefix.
package githubhmac

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/NiekKeijzer/webhook-relay/plugin"
	"github.com/NiekKeijzer/webhook-relay/verify"
)

// Header carries GitHub's signature.
const Header = "X-Hub-Signature-256"

const prefix = "sha256="

// Type is the verifier type "github-hmac-sha256". It takes no params and at
// least one secret.
var Type = verify.Type{
	Name:         "github-hmac-sha256",
	NeedsSecrets: true,
	Validate:     validate,
	New:          newVerifier,
}

// ErrSignature is returned for a missing, malformed or wrong signature.
var ErrSignature = errors.New("github-hmac-sha256: missing, malformed or wrong signature")

type params struct{}

func validate(p plugin.Params) error {
	return p.Decode(&params{})
}

func newVerifier(p plugin.Params, secrets [][]byte) (verify.Verifier, error) {
	if err := validate(p); err != nil {
		return nil, err
	}
	if len(secrets) == 0 {
		return nil, errors.New("github-hmac-sha256 needs at least one secret")
	}
	for _, s := range secrets {
		if len(s) == 0 {
			return nil, errors.New("github-hmac-sha256 secrets cannot be empty")
		}
	}
	return &verifier{secrets: secrets}, nil
}

type verifier struct {
	secrets [][]byte
}

func (v *verifier) Verify(req *plugin.Request) error {
	values := req.Header.Values(Header)
	if len(values) != 1 {
		return ErrSignature
	}
	hexSig, ok := strings.CutPrefix(values[0], prefix)
	if !ok || len(hexSig) != 2*sha256.Size {
		return ErrSignature
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return ErrSignature
	}
	for _, secret := range v.secrets {
		mac := hmac.New(sha256.New, secret)
		mac.Write(req.Body)
		if hmac.Equal(mac.Sum(nil), got) {
			return nil
		}
	}
	return ErrSignature
}

func (v *verifier) Headers() []string {
	return []string{Header}
}
