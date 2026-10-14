package config

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// The raw types mirror the file. Pointers tell a missing key from a zero
// value.
type rawConfig struct {
	Version *int64     `toml:"version"`
	Routes  []rawRoute `toml:"routes"`
}

type rawRoute struct {
	Name             string       `toml:"name"`
	IDs              []string     `toml:"ids"`
	IDsEnv           []string     `toml:"ids_env"`
	Methods          *[]string    `toml:"methods"`
	Verifier         *rawVerifier `toml:"verifier"`
	Acceptor         *rawAcceptor `toml:"acceptor"`
	DeliveryIDHeader *string      `toml:"delivery_id_header"`
	Headers          []string     `toml:"headers"`
	Targets          []rawTarget  `toml:"targets"`
}

type rawVerifier struct {
	Type       string         `toml:"type"`
	Params     map[string]any `toml:"params"`
	SecretsEnv []string       `toml:"secrets_env"`
}

type rawAcceptor struct {
	Type   string         `toml:"type"`
	Params map[string]any `toml:"params"`
}

type rawTarget struct {
	Name string `toml:"name"`
	URL  string `toml:"url"`
}

var (
	namePattern   = regexp.MustCompile(`^[a-z0-9-]+$`)
	uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	envPattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

	allowedMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}

	// Headers the forwarder cannot replay: they describe the connection or
	// the framing of the original request, not the webhook.
	unkeepableHeaders = []string{"Connection", "Content-Length", "Host", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}
)

// ReservedHeaderPrefix starts the names of the headers the forwarder adds.
// They cannot be kept.
const ReservedHeaderPrefix = "X-Relay-"

// errorList collects validation errors.
type errorList []error

func (l *errorList) add(format string, args ...any) {
	*l = append(*l, fmt.Errorf("config: "+format, args...))
}

func (r *rawConfig) validate() (*Config, error) {
	var errs errorList
	cfg := &Config{}

	switch {
	case r.Version == nil:
		errs.add("version is required")
	case *r.Version != 1:
		errs.add("version must be 1")
	}

	names := map[string]bool{}
	ids := map[string]string{} // ID to route
	for i, rr := range r.Routes {
		route := rr.validate(&errs, where(i, rr.Name))
		if namePattern.MatchString(route.Name) {
			if names[route.Name] {
				errs.add("route %q: name is used by more than one route", route.Name)
			}
			names[route.Name] = true
		}
		for j, id := range route.IDs {
			if other, ok := ids[id]; ok {
				errs.add("%s: ids[%d] is also used by route %q", where(i, rr.Name), j, other)
			}
			ids[id] = route.Name
		}
		cfg.Routes = append(cfg.Routes, route)
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

func (rr *rawRoute) validate(errs *errorList, where string) Route {
	route := Route{Name: rr.Name, IDs: rr.IDs, IDsEnv: rr.IDsEnv}
	if !namePattern.MatchString(rr.Name) {
		errs.add("%s: name must match [a-z0-9-]+", where)
	}

	if len(rr.IDs) == 0 && len(rr.IDsEnv) == 0 {
		errs.add("%s: ids or ids_env needs at least one entry", where)
	}
	for j, id := range rr.IDs {
		if !uuidV4Pattern.MatchString(id) {
			errs.add("%s: ids[%d] is not a lowercase version 4 UUID", where, j)
		}
	}
	checkEnvNames(errs, where+": ids_env", rr.IDsEnv)

	route.Methods = []string{http.MethodPost}
	if rr.Methods != nil {
		route.Methods = *rr.Methods
		if len(route.Methods) == 0 {
			errs.add("%s: methods needs at least one entry", where)
		}
		for j, m := range route.Methods {
			if !slices.Contains(allowedMethods, m) {
				errs.add("%s: methods[%d] must be one of %s", where, j, strings.Join(allowedMethods, ", "))
			}
			if slices.Index(route.Methods, m) != j {
				errs.add("%s: methods[%d] is a duplicate", where, j)
			}
		}
	}

	if rr.Verifier == nil {
		errs.add("%s: verifier is required", where)
	} else {
		route.Verifier = Verifier{Type: rr.Verifier.Type, Params: NewParams(rr.Verifier.Params), SecretsEnv: rr.Verifier.SecretsEnv}
		if rr.Verifier.Type == "" {
			errs.add("%s: verifier.type is required", where)
		}
		checkEnvNames(errs, where+": verifier.secrets_env", rr.Verifier.SecretsEnv)
	}

	route.Acceptor = Acceptor{Type: DefaultAcceptor}
	if rr.Acceptor != nil {
		route.Acceptor = Acceptor{Type: rr.Acceptor.Type, Params: NewParams(rr.Acceptor.Params)}
		if rr.Acceptor.Type == "" {
			errs.add("%s: acceptor.type is required", where)
		}
	}

	if rr.DeliveryIDHeader != nil {
		route.DeliveryIDHeader = http.CanonicalHeaderKey(*rr.DeliveryIDHeader)
		checkHeader(errs, where+": delivery_id_header", *rr.DeliveryIDHeader)
	}
	for j, h := range rr.Headers {
		route.Headers = append(route.Headers, http.CanonicalHeaderKey(h))
		checkHeader(errs, fmt.Sprintf("%s: headers[%d]", where, j), h)
	}

	if len(rr.Targets) == 0 {
		errs.add("%s: targets needs at least one entry", where)
	}
	targetNames := map[string]bool{}
	for j, rt := range rr.Targets {
		tw := fmt.Sprintf("%s: targets[%d]", where, j)
		if namePattern.MatchString(rt.Name) {
			tw = fmt.Sprintf("%s: target %q", where, rt.Name)
		} else {
			errs.add("%s: name must match [a-z0-9-]+", tw)
		}
		if targetNames[rt.Name] {
			errs.add("%s: name is used by more than one target of the route", tw)
		}
		targetNames[rt.Name] = true
		u, err := targetURL(rt.URL)
		if err != nil {
			errs.add("%s: url %v", tw, err)
		}
		route.Targets = append(route.Targets, Target{Name: rt.Name, URL: u})
	}
	return route
}

// targetURL parses a target URL. Its errors never contain the URL.
func targetURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	switch {
	case err != nil, u.Scheme != "http" && u.Scheme != "https", u.Opaque != "", u.Hostname() == "":
		return nil, errors.New("must be an absolute http or https URL")
	case u.User != nil:
		return nil, errors.New("must not contain user info")
	case u.RawQuery != "" || u.ForceQuery:
		return nil, errors.New("must not have a query string")
	case strings.Contains(s, "#"):
		return nil, errors.New("must not have a fragment")
	}
	return u, nil
}

func checkHeader(errs *errorList, where, name string) {
	canonical := http.CanonicalHeaderKey(name)
	switch {
	case !isToken(name):
		errs.add("%s: not a valid header name", where)
	case strings.HasPrefix(canonical, ReservedHeaderPrefix):
		errs.add("%s: names starting with %s are reserved for the forwarder", where, ReservedHeaderPrefix)
	case slices.Contains(unkeepableHeaders, canonical) || strings.HasPrefix(canonical, "Proxy-"):
		errs.add("%s: %s cannot be kept", where, canonical)
	}
}

// isToken reports whether s is a valid header field name (RFC 9110, 5.1).
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

func checkEnvNames(errs *errorList, where string, names []string) {
	for j, name := range names {
		switch {
		case !envPattern.MatchString(name):
			errs.add("%s[%d]: not a valid environment variable name", where, j)
		case strings.HasSuffix(name, fileSuffix):
			// NAME_FILE holds the path of NAME's secret file, never a secret.
			errs.add("%s[%d]: names ending in %s are reserved for secret files", where, j, fileSuffix)
		}
		if slices.Index(names, name) != j {
			errs.add("%s[%d]: %q is listed more than once", where, j, name)
		}
	}
}

// checkPlugins runs the plugin checks for the whole file.
func (c *Config) checkPlugins(plugins Plugins) error {
	var errs errorList
	for _, r := range c.Routes {
		vp := r.Verifier.Params.track()
		needsSecrets, err := plugins.Verifier(r.Verifier.Type, vp)
		switch {
		case err != nil:
			errs.add("route %q: verifier: %v", r.Name, err)
		case !vp.decoded():
			errs.add("route %q: verifier: unknown params keys %q", r.Name, sortedKeys(r.Verifier.Params.raw))
		case needsSecrets && len(r.Verifier.SecretsEnv) == 0:
			errs.add("route %q: verifier type %q needs at least one secrets_env entry", r.Name, r.Verifier.Type)
		case !needsSecrets && len(r.Verifier.SecretsEnv) > 0:
			errs.add("route %q: verifier type %q takes no secrets", r.Name, r.Verifier.Type)
		}

		ap := r.Acceptor.Params.track()
		err = plugins.Acceptor(r.Acceptor.Type, ap)
		switch {
		case err != nil:
			errs.add("route %q: acceptor: %v", r.Name, err)
		case !ap.decoded():
			errs.add("route %q: acceptor: unknown params keys %q", r.Name, sortedKeys(r.Acceptor.Params.raw))
		}
	}
	return errors.Join(errs...)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
