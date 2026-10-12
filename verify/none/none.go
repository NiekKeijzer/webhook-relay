// Package none implements the verifier type "none". It accepts every request.
// A route uses it to state that it has no verifier, so a route without
// authentication is a visible choice in the configuration.
package none

import (
	"errors"

	"github.com/NiekKeijzer/webhook-relay/plugin"
	"github.com/NiekKeijzer/webhook-relay/verify"
)

// Type is the verifier type "none". It takes no params and no secrets.
var Type = verify.Type{
	Name:         "none",
	NeedsSecrets: false,
	Validate:     validate,
	New:          newVerifier,
}

type params struct{}

func validate(p plugin.Params) error {
	return p.Decode(&params{})
}

func newVerifier(p plugin.Params, secrets [][]byte) (verify.Verifier, error) {
	if err := validate(p); err != nil {
		return nil, err
	}
	if len(secrets) > 0 {
		return nil, errors.New("none takes no secrets")
	}
	return verifier{}, nil
}

type verifier struct{}

func (verifier) Verify(*plugin.Request) error { return nil }

func (verifier) Headers() []string { return nil }
