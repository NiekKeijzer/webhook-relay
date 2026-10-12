package config

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

// Params holds a plugin's params from the configuration. It implements
// plugin.Params.
type Params struct {
	raw map[string]any
}

// NewParams returns params holding raw, a table as the TOML library decodes
// it into map[string]any.
func NewParams(raw map[string]any) Params {
	return Params{raw: raw}
}

// Decode fills into, a pointer to a struct with `toml` tags, from the params.
// It fails on unknown or mistyped keys.
//
// Each Params is decoded on its own. The library's MetaData.Undecoded lists
// key paths for all routes together, so after one route's plugin decodes a
// key, the same key would count as known on every other route.
func (p Params) Decode(into any) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(p.raw); err != nil {
		return fmt.Errorf("params: %w", err)
	}
	md, err := toml.Decode(buf.String(), into)
	if err != nil {
		var pe toml.ParseError
		if errors.As(err, &pe) {
			// The message can quote the value.
			return fmt.Errorf("params: key %q: value out of range", pe.LastKey)
		}
		return fmt.Errorf("params: %s", stripPosition(err.Error()))
	}
	var unknown []string
	for _, k := range md.Undecoded() {
		unknown = append(unknown, k.String())
	}
	// The library matches keys to fields regardless of case; plugin tags are
	// lowercase.
	for _, k := range md.Keys() {
		if s := k.String(); s != strings.ToLower(s) {
			unknown = append(unknown, s)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("params: unknown keys %q", unknown)
	}
	return nil
}

// stripPosition removes the "toml: line N " prefix of a library error. The
// line refers to the re-encoded params, not to the configuration file.
func stripPosition(msg string) string {
	if rest, ok := strings.CutPrefix(msg, "toml: line "); ok {
		if _, after, ok := strings.Cut(rest, " "); ok {
			return after
		}
	}
	return msg
}

// tracked records whether a plugin decoded its params, so a plugin that
// ignores its params cannot accept unknown keys.
type tracked struct {
	params Params
	ok     bool
}

func (p Params) track() *tracked {
	return &tracked{params: p}
}

func (t *tracked) Decode(into any) error {
	err := t.params.Decode(into)
	if err == nil {
		t.ok = true
	}
	return err
}

func (t *tracked) decoded() bool {
	return t.ok || len(t.params.raw) == 0
}
