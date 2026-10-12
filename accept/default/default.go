// Package defaultaccept implements the acceptor type "default" (§6.4 of the
// architecture). It enqueues every request and replies with an empty body
// and the status in params.status: 202 unless set, and always a 2xx.
//
// The directory is accept/default; the package has another name because
// default is a Go keyword.
package defaultaccept

import (
	"fmt"
	"net/http"

	"github.com/NiekKeijzer/webhook-relay/accept"
	"github.com/NiekKeijzer/webhook-relay/plugin"
)

// DefaultStatus is the reply status when params.status is not set.
const DefaultStatus = http.StatusAccepted

// Type is the acceptor type "default".
var Type = accept.Type{
	Name:     "default",
	Validate: validate,
	New:      newAcceptor,
}

type params struct {
	Status *int `toml:"status"`
}

func decode(p plugin.Params) (int, error) {
	var ps params
	if err := p.Decode(&ps); err != nil {
		return 0, err
	}
	if ps.Status == nil {
		return DefaultStatus, nil
	}
	if *ps.Status < 200 || *ps.Status > 299 {
		return 0, fmt.Errorf("status %d is not a 2xx status", *ps.Status)
	}
	return *ps.Status, nil
}

func validate(p plugin.Params) error {
	_, err := decode(p)
	return err
}

func newAcceptor(p plugin.Params) (accept.Acceptor, error) {
	status, err := decode(p)
	if err != nil {
		return nil, err
	}
	return acceptor{status: status}, nil
}

type acceptor struct {
	status int
}

func (a acceptor) Accept(*plugin.Request) accept.Decision {
	return accept.Decision{Enqueue: true, Status: a.status}
}
