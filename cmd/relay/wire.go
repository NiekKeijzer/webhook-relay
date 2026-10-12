package main

import (
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/NiekKeijzer/webhook-relay/accept"
	defaultaccept "github.com/NiekKeijzer/webhook-relay/accept/default"
	"github.com/NiekKeijzer/webhook-relay/config"
	"github.com/NiekKeijzer/webhook-relay/forwarder"
	"github.com/NiekKeijzer/webhook-relay/plugin"
	"github.com/NiekKeijzer/webhook-relay/queue"
	"github.com/NiekKeijzer/webhook-relay/receiver"
	"github.com/NiekKeijzer/webhook-relay/verify"
	"github.com/NiekKeijzer/webhook-relay/verify/githubhmac"
	"github.com/NiekKeijzer/webhook-relay/verify/none"
)

// queueTypes are the queue backends compiled into the binary. None exists
// yet, so every configuration fails with an unknown queue type.
var queueTypes []queue.Type

// registries holds the plugin and queue types compiled into the binary. It
// implements config.Plugins and config.Queues.
type registries struct {
	verifiers *verify.Registry
	acceptors *accept.Registry
	queues    map[string]queue.Type
}

func newRegistries(queueTypes []queue.Type) (*registries, error) {
	verifiers, err := verify.NewRegistry(none.Type, githubhmac.Type)
	if err != nil {
		return nil, err
	}
	acceptors, err := accept.NewRegistry(defaultaccept.Type)
	if err != nil {
		return nil, err
	}
	queues := map[string]queue.Type{}
	for _, t := range queueTypes {
		if _, ok := queues[t.Name]; ok {
			return nil, fmt.Errorf("queue type %q registered twice", t.Name)
		}
		queues[t.Name] = t
	}
	return &registries{verifiers: verifiers, acceptors: acceptors, queues: queues}, nil
}

func (r *registries) Verifier(typ string, params plugin.Params) (bool, error) {
	t, ok := r.verifiers.Lookup(typ)
	if !ok {
		return false, fmt.Errorf("unknown verifier type %q", typ)
	}
	return t.NeedsSecrets, t.Validate(params)
}

func (r *registries) Acceptor(typ string, params plugin.Params) error {
	t, ok := r.acceptors.Lookup(typ)
	if !ok {
		return fmt.Errorf("unknown acceptor type %q", typ)
	}
	return t.Validate(params)
}

func (r *registries) QueueNames(typ string) ([]string, []string, error) {
	t, ok := r.queues[typ]
	if !ok {
		return nil, nil, fmt.Errorf("unknown queue type %q", typ)
	}
	return t.Params, t.Credentials, nil
}

func (r *registries) ValidateQueue(typ string, params map[string]string) (time.Duration, error) {
	t, ok := r.queues[typ]
	if !ok {
		return 0, fmt.Errorf("unknown queue type %q", typ)
	}
	return t.Validate(params)
}

// newReceiver builds the receiver's handler from a validated configuration.
// The body limit is the queue's maximum message size: a larger body can
// never be enqueued.
func newReceiver(cfg *config.Config, regs *registries, env *config.ReceiverEnv, sender queue.Sender, logger *slog.Logger) (*receiver.Handler, error) {
	var routes []receiver.Route
	for _, r := range cfg.Routes {
		vt, ok := regs.verifiers.Lookup(r.Verifier.Type)
		if !ok {
			return nil, fmt.Errorf("route %q: unknown verifier type %q", r.Name, r.Verifier.Type)
		}
		v, err := vt.New(r.Verifier.Params, env.RouteSecrets[r.Name])
		if err != nil {
			return nil, fmt.Errorf("route %q: verifier: %w", r.Name, err)
		}
		at, ok := regs.acceptors.Lookup(r.Acceptor.Type)
		if !ok {
			return nil, fmt.Errorf("route %q: unknown acceptor type %q", r.Name, r.Acceptor.Type)
		}
		a, err := at.New(r.Acceptor.Params)
		if err != nil {
			return nil, fmt.Errorf("route %q: acceptor: %w", r.Name, err)
		}
		var targets []string
		for _, t := range r.Targets {
			targets = append(targets, t.Name)
		}
		routes = append(routes, receiver.Route{
			Name:             r.Name,
			IDs:              r.IDs,
			Methods:          r.Methods,
			Verifier:         v,
			Acceptor:         a,
			DeliveryIDHeader: r.DeliveryIDHeader,
			Headers:          r.Headers,
			Targets:          targets,
		})
	}
	return receiver.New(receiver.Options{
		Routes:       routes,
		Queue:        sender,
		Key:          env.EnvelopeKey,
		MaxBodyBytes: int64(sender.MaxMessageSize()),
		Logger:       logger,
	})
}

// newForwarder builds the forwarder from a validated configuration.
func newForwarder(cfg *config.Config, env *config.ForwarderEnv, rcv queue.Receiver, logger *slog.Logger) (*forwarder.Forwarder, error) {
	targets := map[string]map[string]*url.URL{}
	for _, r := range cfg.Routes {
		targets[r.Name] = map[string]*url.URL{}
		for _, t := range r.Targets {
			targets[r.Name][t.Name] = t.URL
		}
	}
	return forwarder.New(forwarder.Options{
		Queue:        rcv,
		Keys:         env.EnvelopeKeys,
		Targets:      targets,
		Timeout:      env.Timeout,
		DedupeWindow: env.DedupeWindow,
		Logger:       logger,
	})
}
