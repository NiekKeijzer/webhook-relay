// Package config parses and validates the relay's configuration file (§10 of
// the architecture) and reads the environment variables each component uses.
//
// Errors never quote values from the file or the environment, such as route
// IDs, target URLs or secrets. They name fields, routes, targets, environment
// variables and line numbers.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/NiekKeijzer/webhook-relay/plugin"
)

// Config is a parsed and validated configuration file.
type Config struct {
	Routes []Route
}

// Route is one configured webhook endpoint.
type Route struct {
	Name             string
	IDs              []string
	Methods          []string
	Verifier         Verifier
	Acceptor         Acceptor
	DeliveryIDHeader string   // canonical form; empty if the route has none
	Headers          []string // canonical form
	Targets          []Target
}

// Verifier selects a route's verifier.
type Verifier struct {
	Type   string
	Params Params
	// SecretsEnv names the environment variables holding the route's active
	// secrets.
	SecretsEnv []string
}

// Acceptor selects a route's acceptor.
type Acceptor struct {
	Type   string
	Params Params
}

// Target is a named destination of a route.
type Target struct {
	Name string
	URL  *url.URL
}

// DefaultAcceptor is the acceptor type of a route without an acceptor.
const DefaultAcceptor = "default"

// Plugins checks verifier and acceptor types and their params. The config
// package cannot import their registries (§11), so the caller passes them in.
type Plugins interface {
	// Verifier checks a verifier's params and reports whether its type needs
	// secrets.
	Verifier(typ string, params plugin.Params) (needsSecrets bool, err error)
	// Acceptor checks an acceptor's params.
	Acceptor(typ string, params plugin.Params) error
}

// Parse parses and validates a configuration file. It checks the whole file,
// including the parts only the other component uses, so both components
// accept or reject the same file. It resolves no environment variables.
func Parse(data []byte, plugins Plugins) (*Config, error) {
	var raw rawConfig
	md, err := toml.Decode(string(data), &raw)
	if err != nil {
		return nil, decodeError(err)
	}
	if err := checkKeys(md); err != nil {
		return nil, err
	}
	var generic map[string]any
	if _, err := toml.Decode(string(data), &generic); err != nil {
		return nil, decodeError(err)
	}
	if err := checkInlineTargets(generic); err != nil {
		return nil, err
	}
	cfg, err := raw.validate()
	if err != nil {
		return nil, err
	}
	if err := cfg.checkPlugins(plugins); err != nil {
		return nil, err
	}
	return cfg, nil
}

// decodeError describes a TOML error without quoting the file. The library's
// messages for syntax and range errors can contain parts of the file, which
// may hold route IDs or target URLs (invariant 7).
func decodeError(err error) error {
	var pe toml.ParseError
	if errors.As(err, &pe) {
		return fmt.Errorf("config: line %d, column %d (last key %q): invalid TOML or value out of range",
			pe.Position.Line, pe.Position.Col, pe.LastKey)
	}
	// Other errors are type mismatches, which name keys and types only.
	return fmt.Errorf("config: %w", err)
}

// checkKeys rejects keys that are not part of the schema. Plugin params are
// checked elsewhere.
func checkKeys(md toml.MetaData) error {
	var errs []error
	seen := map[string]bool{}
	report := func(k toml.Key) {
		if s := k.String(); !seen[s] {
			seen[s] = true
			errs = append(errs, fmt.Errorf("config: unknown key %q", s))
		}
	}
	for _, k := range md.Undecoded() {
		report(k)
	}
	// The library matches keys to fields regardless of case, so "Name" fills
	// "name". The schema's keys are all lowercase.
	for _, k := range md.Keys() {
		if !freeForm(k) && k.String() != strings.ToLower(k.String()) {
			report(k)
		}
	}
	return errors.Join(errs...)
}

// freeForm reports whether k is inside a table whose keys the schema does not
// define: plugin params.
func freeForm(k toml.Key) bool {
	return len(k) >= 4 && k[0] == "routes" && (k[1] == "verifier" || k[1] == "acceptor") && k[2] == "params"
}

// checkInlineTargets rejects targets written as [[routes.targets]] blocks. A
// block belongs to whichever [[routes]] comes before it, so a misplaced block
// silently moves a target to another route. The library decodes blocks into
// []map[string]any and inline arrays into []any. MetaData.Type cannot tell
// them apart per route, because it reports one type per key path for all
// routes together.
func checkInlineTargets(generic map[string]any) error {
	var routes []map[string]any
	switch rs := generic["routes"].(type) {
	case []map[string]any:
		routes = rs
	case []any:
		for _, r := range rs {
			if m, ok := r.(map[string]any); ok {
				routes = append(routes, m)
			}
		}
	}
	var errs []error
	for i, r := range routes {
		if _, ok := r["targets"].([]map[string]any); ok {
			errs = append(errs, fmt.Errorf("config: %s: targets must be an inline array, not [[routes.targets]] blocks", where(i, r["name"])))
		}
	}
	return errors.Join(errs...)
}

// where names a route in an error: by name when it has a valid one, else by
// position.
func where(i int, name any) string {
	if s, ok := name.(string); ok && namePattern.MatchString(s) {
		return fmt.Sprintf("route %q", s)
	}
	return fmt.Sprintf("routes[%d]", i)
}
