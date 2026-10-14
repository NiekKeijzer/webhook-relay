package config

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MinEnvelopeKeySize is the minimum length of an envelope key, in bytes.
const MinEnvelopeKeySize = 32

// The environment variables with fixed names (§10.3).
const (
	envQueueType           = "RELAY_QUEUE_TYPE"
	envQueuePrefix         = "RELAY_QUEUE_" // followed by a backend's param or credential name
	envEnvelopeKey         = "RELAY_ENVELOPE_KEY"
	envEnvelopeKeyPrevious = "RELAY_ENVELOPE_KEY_PREVIOUS"
	envForwarderTimeout    = "RELAY_FORWARDER_TIMEOUT_SECONDS"
	envForwarderDedupe     = "RELAY_FORWARDER_DEDUPE_WINDOW_SECONDS"

	// fileSuffix turns a secret's variable name into the name of the variable
	// that holds the path of a file with the secret.
	fileSuffix = "_FILE"
)

// queueNamePattern matches the names of a queue backend's params and
// credentials.
var queueNamePattern = regexp.MustCompile(`^[A-Z0-9_]+$`)

// LookupFunc looks up an environment variable, like os.LookupEnv.
type LookupFunc func(name string) (value string, ok bool)

// Queues looks up queue backends. The config package cannot import the queue
// package (§11), so the caller passes this in.
type Queues interface {
	// QueueNames returns the names of a queue type's params and credentials.
	QueueNames(typ string) (params, credentials []string, err error)
	// ValidateQueue checks a queue type's params and returns its ack deadline.
	ValidateQueue(typ string, params map[string]string) (ackDeadline time.Duration, err error)
}

// Queue is the queue backend and its settings.
type Queue struct {
	Type        string
	Params      map[string]string // by param name; unset params are left out
	Credentials map[string]string // by credential name
}

// ReceiverEnv holds what the receiver reads from the environment.
type ReceiverEnv struct {
	Queue Queue
	// EnvelopeKey signs envelopes.
	EnvelopeKey []byte
	// RouteSecrets holds each route's active secrets, by route name.
	RouteSecrets map[string][][]byte
	// RouteIDs holds the IDs read from each route's ids_env, by route name.
	RouteIDs map[string][]string
}

// ForwarderEnv holds what the forwarder reads from the environment.
type ForwarderEnv struct {
	Queue Queue
	// EnvelopeKeys holds the current envelope key and, during a rotation, the
	// previous one.
	EnvelopeKeys [][]byte
	Timeout      time.Duration
	DedupeWindow time.Duration
}

// ResolveReceiver reads the environment variables the receiver uses: the
// queue settings with its credentials, the envelope key, the route secrets
// and the route IDs from ids_env.
func (c *Config) ResolveReceiver(lookup LookupFunc, queues Queues) (*ReceiverEnv, error) {
	r := resolver{lookup: lookup}
	q, _ := r.queue(queues)
	env := &ReceiverEnv{
		Queue:        q,
		EnvelopeKey:  r.envelopeKey(envEnvelopeKey),
		RouteSecrets: map[string][][]byte{},
		RouteIDs:     map[string][]string{},
	}
	ids := map[string]string{} // ID to route; Parse checked the file's IDs
	for _, route := range c.Routes {
		for _, id := range route.IDs {
			ids[id] = route.Name
		}
	}
	for _, route := range c.Routes {
		var secrets [][]byte
		for _, name := range route.Verifier.SecretsEnv {
			secrets = append(secrets, r.secret(name))
		}
		env.RouteSecrets[route.Name] = secrets
		for _, name := range route.IDsEnv {
			// Read like a secret so NAME_FILE works and errors never quote it.
			id := string(r.secret(name))
			switch {
			case id == "":
				// r.secret reported it.
			case !uuidV4Pattern.MatchString(id):
				r.errs.add("route %q: the ID in %s is not a lowercase version 4 UUID", route.Name, name)
			default:
				if other, ok := ids[id]; ok {
					r.errs.add("route %q: the ID in %s is also used by route %q", route.Name, name, other)
				}
				ids[id] = route.Name
				env.RouteIDs[route.Name] = append(env.RouteIDs[route.Name], id)
			}
		}
	}
	if err := errors.Join(r.errs...); err != nil {
		return nil, err
	}
	return env, nil
}

// ResolveForwarder reads the environment variables the forwarder uses: the
// queue settings with its credentials, the envelope keys and the forward
// timeout and dedupe window. None of them depends on the configuration file.
func ResolveForwarder(lookup LookupFunc, queues Queues) (*ForwarderEnv, error) {
	r := resolver{lookup: lookup}
	q, ackDeadline := r.queue(queues)
	env := &ForwarderEnv{
		Queue:        q,
		EnvelopeKeys: [][]byte{r.envelopeKey(envEnvelopeKey)},
		Timeout:      r.seconds(envForwarderTimeout),
		DedupeWindow: r.seconds(envForwarderDedupe),
	}
	if r.isSet(envEnvelopeKeyPrevious) {
		env.EnvelopeKeys = append(env.EnvelopeKeys, r.envelopeKey(envEnvelopeKeyPrevious))
	}
	if env.Timeout != 0 && ackDeadline != 0 && env.Timeout >= ackDeadline {
		r.errs.add("%s must be shorter than the queue's ack deadline (%s)", envForwarderTimeout, ackDeadline)
	}
	if err := errors.Join(r.errs...); err != nil {
		return nil, err
	}
	return env, nil
}

// resolver reads environment variables and collects errors. Errors name
// variables, never their values.
type resolver struct {
	lookup LookupFunc
	errs   errorList
}

// value returns a variable's value, or "" if it is unset.
func (r *resolver) value(name string) string {
	v, _ := r.lookup(name)
	return v
}

func (r *resolver) required(name string) string {
	v := r.value(name)
	if v == "" {
		r.errs.add("environment variable %s is missing or empty", name)
	}
	return v
}

// isSet reports whether a secret is given, in its variable or as a file.
func (r *resolver) isSet(name string) bool {
	return r.value(name) != "" || r.value(name+fileSuffix) != ""
}

// secret returns a secret: the raw bytes of the variable name, or the content
// of the file named by name_FILE without trailing line breaks. Errors do not
// quote the path either, in case it holds a misplaced secret.
func (r *resolver) secret(name string) []byte {
	v, path := r.value(name), r.value(name+fileSuffix)
	switch {
	case v != "" && path != "":
		r.errs.add("set either %s or %s%s, not both", name, name, fileSuffix)
		return nil
	case path != "":
		data, err := os.ReadFile(path)
		if err != nil {
			reason := "read failed"
			if pe, ok := errors.AsType[*fs.PathError](err); ok {
				reason = pe.Err.Error()
			}
			r.errs.add("cannot read the file named by %s%s: %s", name, fileSuffix, reason)
			return nil
		}
		if v = strings.TrimRight(string(data), "\r\n"); v == "" {
			r.errs.add("the file named by %s%s is empty", name, fileSuffix)
		}
	case v == "":
		r.errs.add("environment variable %s (or %s%s) is missing or empty", name, name, fileSuffix)
	}
	return []byte(v)
}

func (r *resolver) envelopeKey(name string) []byte {
	key := r.secret(name)
	if len(key) > 0 && len(key) < MinEnvelopeKeySize {
		r.errs.add("envelope key in %s is shorter than %d bytes", name, MinEnvelopeKeySize)
	}
	return key
}

// seconds reads a whole number of seconds, at least 1.
func (r *resolver) seconds(name string) time.Duration {
	v := r.required(name)
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	switch {
	case errors.Is(err, strconv.ErrRange), err == nil && n > math.MaxInt64/int64(time.Second):
		r.errs.add("environment variable %s is too large", name)
	case err != nil || n < 1:
		r.errs.add("environment variable %s must be a whole number of seconds, at least 1", name)
	default:
		return time.Duration(n) * time.Second
	}
	return 0
}

// queue reads the queue type and the backend's params and credentials, and
// returns them with the queue's ack deadline. The ack deadline is 0 if the
// settings are invalid.
func (r *resolver) queue(queues Queues) (Queue, time.Duration) {
	q := Queue{Type: r.required(envQueueType)}
	if q.Type == "" {
		return q, 0
	}
	params, credentials, err := queues.QueueNames(q.Type)
	if err != nil {
		r.errs.add("queue: %v", err)
		return q, 0
	}
	if err := checkQueueNames(slices.Concat(params, credentials)); err != nil {
		r.errs.add("queue: type %q: %v", q.Type, err)
		return q, 0
	}
	q.Params = map[string]string{}
	for _, name := range params {
		if v := r.value(envQueuePrefix + name); v != "" {
			q.Params[name] = v
		}
	}
	q.Credentials = map[string]string{}
	for _, name := range credentials {
		q.Credentials[name] = string(r.secret(envQueuePrefix + name))
	}
	ackDeadline, err := queues.ValidateQueue(q.Type, q.Params)
	switch {
	case err != nil:
		r.errs.add("queue: %v", err)
		return q, 0
	case ackDeadline <= 0:
		r.errs.add("queue: type %q reports no ack deadline", q.Type)
		return q, 0
	}
	return q, ackDeadline
}

// checkQueueNames checks the names a queue backend declares for its params
// and credentials. A bad name is a bug in the backend.
func checkQueueNames(names []string) error {
	for i, name := range names {
		switch {
		case !queueNamePattern.MatchString(name), name == "TYPE", strings.HasSuffix(name, fileSuffix):
			return fmt.Errorf("invalid setting name %q", name)
		case slices.Index(names, name) != i:
			return fmt.Errorf("setting %q is declared twice", name)
		}
	}
	return nil
}
