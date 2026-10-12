// Package accept defines the acceptor contract (§6.4 of the architecture) and
// a registry of acceptor types. Implementations live in accept/<type>.
package accept

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/NiekKeijzer/webhook-relay/plugin"
)

// Decision is how the receiver answers the sender.
type Decision struct {
	Enqueue bool // false: reply without enqueueing, e.g. to a handshake
	Status  int  // must be 2xx when Enqueue is true
	Header  http.Header
	Body    []byte
}

// Acceptor decides how the receiver answers a verified request and whether
// it is enqueued.
type Acceptor interface {
	Accept(req *plugin.Request) Decision
}

// Type is an acceptor type, selected by name in the configuration.
type Type struct {
	Name string
	// Validate checks a route's params. Both components run it at startup.
	Validate func(params plugin.Params) error
	// New builds the acceptor for one route.
	New func(params plugin.Params) (Acceptor, error)
}

// Registry holds the acceptor types compiled into a binary.
type Registry struct {
	types map[string]Type
}

// NewRegistry returns a registry of types. Names must be unique.
func NewRegistry(types ...Type) (*Registry, error) {
	r := &Registry{types: map[string]Type{}}
	for _, t := range types {
		switch {
		case t.Name == "" || t.Validate == nil || t.New == nil:
			return nil, errors.New("accept: incomplete acceptor type")
		case r.types[t.Name].Name != "":
			return nil, fmt.Errorf("accept: acceptor type %q registered twice", t.Name)
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
