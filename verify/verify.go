// Package verify defines the verifier contract (§6.3 of the architecture) and
// a registry of verifier types. Implementations live in verify/<type>.
package verify

import (
	"errors"
	"fmt"

	"github.com/NiekKeijzer/webhook-relay/plugin"
)

// Verifier decides whether a request is authentic.
type Verifier interface {
	// Verify returns nil when the request is authentic.
	Verify(req *plugin.Request) error
	// Headers lists the request headers the scheme uses. The receiver keeps
	// them in the envelope, so a target can verify the request itself.
	Headers() []string
}

// Type is a verifier type, selected by name in the configuration.
type Type struct {
	Name string
	// NeedsSecrets reports whether a route using this type lists at least one
	// secret (true) or none (false). The configuration is checked against it
	// without resolving secrets, so both components reject the same files.
	NeedsSecrets bool
	// Validate checks a route's params without secrets. Both components run
	// it at startup, so they accept or reject the same configuration.
	Validate func(params plugin.Params) error
	// New builds the verifier for one route. secrets holds every active
	// secret of the route; a match with any one of them is enough.
	New func(params plugin.Params, secrets [][]byte) (Verifier, error)
}

// Registry holds the verifier types compiled into a binary.
type Registry struct {
	types map[string]Type
}

// NewRegistry returns a registry of types. Names must be unique.
func NewRegistry(types ...Type) (*Registry, error) {
	r := &Registry{types: map[string]Type{}}
	for _, t := range types {
		switch {
		case t.Name == "" || t.Validate == nil || t.New == nil:
			return nil, errors.New("verify: incomplete verifier type")
		case r.types[t.Name].Name != "":
			return nil, fmt.Errorf("verify: verifier type %q registered twice", t.Name)
		}
		r.types[t.Name] = t
	}
	return r, nil
}

// Lookup returns the type with the given name.
func (r *Registry) Lookup(name string) (Type, bool) {
	t, ok := r.types[name]
	return t, ok
}
