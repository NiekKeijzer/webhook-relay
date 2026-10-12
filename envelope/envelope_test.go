package envelope

import (
	"bytes"
	"compress/flate"
	"crypto/rand"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	keyA = bytes.Repeat([]byte("a"), 32)
	keyB = bytes.Repeat([]byte("b"), 32)
)

func sample() *Envelope {
	return &Envelope{
		Route:               "github-infra",
		Target:              "deploy",
		DeliveryID:          "72d3162e-cc78-11e3-81ab-4c9367dc0958",
		DeliveryIDGenerated: false,
		ReceivedAt:          time.Date(2026, 10, 12, 15, 4, 5, 123456789, time.UTC),
		Method:              "POST",
		Query:               "a=1&b=%20&a=2",
		Header: http.Header{
			"Content-Type":        {"application/json"},
			"X-Hub-Signature-256": {"sha256=abc"},
			"X-Multi":             {"one", "", "three"},
		},
		Body: []byte(`{"zen":"Keep it logically awesome."}`),
	}
}

func TestRoundTrip(t *testing.T) {
	binary := make([]byte, 4096)
	rand.Read(binary)
	tests := []struct {
		name   string
		change func(*Envelope)
	}{
		{"sample", func(*Envelope) {}},
		{"binary body", func(e *Envelope) { e.Body = binary }},
		{"empty body and header", func(e *Envelope) { e.Body = nil; e.Header = http.Header{}; e.Query = "" }},
		{"generated delivery ID", func(e *Envelope) { e.DeliveryIDGenerated = true }},
		{"utf-8 and nul in body", func(e *Envelope) { e.Body = []byte("héllo\x00wörld\xff") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := sample()
			tt.change(in)
			msg, err := Seal(in, keyA)
			if err != nil {
				t.Fatal(err)
			}
			out, err := Open(msg, [][]byte{keyA})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Body, in.Body) {
				t.Errorf("body changed: got %q, want %q", out.Body, in.Body)
			}
			if !out.ReceivedAt.Equal(in.ReceivedAt) {
				t.Errorf("received at = %v, want %v", out.ReceivedAt, in.ReceivedAt)
			}
			out.Body, in.Body = nil, nil
			out.ReceivedAt, in.ReceivedAt = time.Time{}, time.Time{}
			if !reflect.DeepEqual(out, in) {
				t.Errorf("got  %+v\nwant %+v", out, in)
			}
		})
	}
}

func TestSealIsDeterministic(t *testing.T) {
	a, _ := Seal(sample(), keyA)
	b, _ := Seal(sample(), keyA)
	if !bytes.Equal(a, b) {
		t.Error("the same envelope encodes differently")
	}
}

func TestCompression(t *testing.T) {
	e := sample()
	e.Body = []byte(strings.Repeat(`{"commit":"0123456789abcdef","message":"fix"},`, 2000))
	msg, err := Seal(e, keyA)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg) > len(e.Body)/10 {
		t.Errorf("message of %d bytes for a body of %d bytes", len(msg), len(e.Body))
	}
}

func TestOpenWithSeveralKeys(t *testing.T) {
	msg, _ := Seal(sample(), keyB)
	if _, err := Open(msg, [][]byte{keyA, keyB}); err != nil {
		t.Errorf("second key: %v", err)
	}
	if _, err := Open(msg, [][]byte{keyA}); !errors.Is(err, ErrSignature) {
		t.Errorf("other key: error %v, want ErrSignature", err)
	}
	if _, err := Open(msg, nil); !errors.Is(err, ErrSignature) {
		t.Errorf("no keys: error %v, want ErrSignature", err)
	}
	if _, err := Open(msg, [][]byte{{}}); !errors.Is(err, ErrSignature) {
		t.Errorf("empty key: error %v, want ErrSignature", err)
	}
}

func TestSealEmptyKey(t *testing.T) {
	if _, err := Seal(sample(), nil); err == nil {
		t.Error("no error for an empty key")
	}
}

// resign signs a modified message, as a holder of the key could.
func resign(msg []byte) []byte {
	copy(msg[:sigSize], sign(keyA, msg[sigSize:]))
	return msg
}

func TestOpenErrors(t *testing.T) {
	good, _ := Seal(sample(), keyA)
	clone := func() []byte { return bytes.Clone(good) }
	tests := []struct {
		name string
		msg  []byte
		want error
	}{
		{"empty", nil, ErrSignature},
		{"signature only", good[:sigSize], ErrSignature},
		{"tampered signature", func() []byte { m := clone(); m[0] ^= 1; return m }(), ErrSignature},
		{"tampered version", func() []byte { m := clone(); m[sigSize] = 2; return m }(), ErrSignature},
		{"tampered body", func() []byte { m := clone(); m[len(m)-1] ^= 1; return m }(), ErrSignature},
		{"truncated", good[:len(good)-1], ErrSignature},
		{"unknown version", func() []byte { m := clone(); m[sigSize] = 2; return resign(m) }(), ErrVersion},
		{"not deflate", resign(append(clone()[:sigSize+1], 0xff, 0xff, 0xff)), ErrMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Open(tt.msg, [][]byte{keyA}); !errors.Is(err, tt.want) {
				t.Errorf("error %v, want %v", err, tt.want)
			}
		})
	}
}

// sealFields seals already encoded fields, to test the decoder.
func sealFields(t *testing.T, fields []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(make([]byte, sigSize))
	buf.WriteByte(Version)
	zw, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	zw.Write(fields)
	zw.Close()
	return resign(buf.Bytes())
}

func TestDecodeMalformedFields(t *testing.T) {
	fields := encodeFields(sample())
	tests := []struct {
		name   string
		fields []byte
	}{
		{"empty", nil},
		{"truncated", fields[:len(fields)-1]},
		{"trailing data", append(bytes.Clone(fields), 0)},
		{"huge length", []byte{0xff, 0xff, 0xff, 0xff, 0x0f}},
		{"unknown flag", func() []byte {
			f := bytes.Clone(fields)
			// route, target and delivery ID are length-prefixed with one byte each.
			i := 1 + int(f[0])
			i += 1 + int(f[i])
			i += 1 + int(f[i])
			f[i] |= 2
			return f
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Open(sealFields(t, tt.fields), [][]byte{keyA}); !errors.Is(err, ErrMalformed) {
				t.Errorf("error %v, want ErrMalformed", err)
			}
		})
	}
}
