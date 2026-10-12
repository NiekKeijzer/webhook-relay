// Package plugin holds the types that verifiers and acceptors share: the
// request as the receiver read it, and a plugin's params from the
// configuration.
package plugin

import "net/http"

// Request is the incoming request as the receiver read it.
type Request struct {
	Method string
	Query  string // raw query string, unchanged
	Header http.Header
	Body   []byte // raw bytes, unparsed
}

// Params holds a plugin's params from the configuration. Decode fills a
// struct defined by the plugin and fails on unknown or mistyped keys.
//
// Params are TOML, so the struct's fields use `toml` tags. Keys in the
// configuration match tags exactly, so tags must be lowercase.
type Params interface {
	Decode(into any) error
}
