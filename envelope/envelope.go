// Package envelope encodes, signs, checks and decodes queue messages (§7 of
// the architecture). Both components use it; it is the only place that knows
// the byte layout.
//
// A version 1 message is laid out as:
//
//	signature  32 bytes  HMAC-SHA256 over everything after it
//	version     1 byte   1
//	fields               DEFLATE-compressed, see encodeFields
//
// The signature comes first and covers the version, so a message is checked
// before anything in it is read or decompressed.
package envelope

import (
	"bytes"
	"compress/flate"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"
)

// Version is the format version this package writes and reads.
const Version = 1

const (
	sigSize = sha256.Size
	// maxDecoded bounds the decompressed fields of one message.
	maxDecoded = 64 << 20
)

var (
	ErrSignature = errors.New("envelope: missing or invalid signature")
	ErrVersion   = errors.New("envelope: unknown version")
	ErrMalformed = errors.New("envelope: malformed")
)

// Envelope is one captured request, addressed to one target. It holds names,
// never a route ID, a target URL or a secret.
type Envelope struct {
	Route  string
	Target string
	// DeliveryID identifies the request; DeliveryIDGenerated reports whether
	// the receiver generated it or took it from the sender.
	DeliveryID          string
	DeliveryIDGenerated bool
	// ReceivedAt is when the receiver accepted the request.
	ReceivedAt time.Time
	Method     string
	Query      string // raw query string
	Header     http.Header
	Body       []byte
}

// Seal encodes e and signs it with key.
func Seal(e *Envelope, key []byte) ([]byte, error) {
	if len(key) == 0 {
		return nil, errors.New("envelope: empty key")
	}
	var buf bytes.Buffer
	buf.Write(make([]byte, sigSize))
	buf.WriteByte(Version)
	zw, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(encodeFields(e)); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	msg := buf.Bytes()
	copy(msg[:sigSize], sign(key, msg[sigSize:]))
	return msg, nil
}

// Open checks msg's signature against keys and decodes it. A match with any
// key is enough. It returns an error wrapping ErrSignature, ErrVersion or
// ErrMalformed.
func Open(msg []byte, keys [][]byte) (*Envelope, error) {
	if len(msg) <= sigSize || !checkSignature(msg[:sigSize], msg[sigSize:], keys) {
		return nil, ErrSignature
	}
	signed := msg[sigSize:]
	if signed[0] != Version {
		return nil, ErrVersion
	}
	data, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(signed[1:])), maxDecoded+1))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot decompress", ErrMalformed)
	}
	if len(data) > maxDecoded {
		return nil, fmt.Errorf("%w: too large", ErrMalformed)
	}
	return decodeFields(data)
}

func sign(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func checkSignature(sig, data []byte, keys [][]byte) bool {
	for _, key := range keys {
		if len(key) > 0 && hmac.Equal(sig, sign(key, data)) {
			return true
		}
	}
	return false
}

// encodeFields writes the fields in this order, strings and byte slices
// prefixed with their length as a uvarint:
//
//	route, target, delivery ID   string
//	flags                        1 byte; bit 0: delivery ID generated
//	received at                  varint, Unix nanoseconds
//	method, query                string
//	header                       uvarint count of names; per name, sorted:
//	                             name string, uvarint count of values, values
//	body                         bytes
func encodeFields(e *Envelope) []byte {
	var b []byte
	str := func(s string) {
		b = binary.AppendUvarint(b, uint64(len(s)))
		b = append(b, s...)
	}
	str(e.Route)
	str(e.Target)
	str(e.DeliveryID)
	var flags byte
	if e.DeliveryIDGenerated {
		flags |= 1
	}
	b = append(b, flags)
	b = binary.AppendVarint(b, e.ReceivedAt.UnixNano())
	str(e.Method)
	str(e.Query)
	names := make([]string, 0, len(e.Header))
	for name := range e.Header {
		names = append(names, name)
	}
	slices.Sort(names)
	b = binary.AppendUvarint(b, uint64(len(names)))
	for _, name := range names {
		str(name)
		b = binary.AppendUvarint(b, uint64(len(e.Header[name])))
		for _, v := range e.Header[name] {
			str(v)
		}
	}
	b = binary.AppendUvarint(b, uint64(len(e.Body)))
	b = append(b, e.Body...)
	return b
}

func decodeFields(data []byte) (*Envelope, error) {
	d := decoder{data: data}
	e := &Envelope{}
	e.Route = d.string()
	e.Target = d.string()
	e.DeliveryID = d.string()
	flags := d.byte()
	e.DeliveryIDGenerated = flags&1 != 0
	e.ReceivedAt = time.Unix(0, d.varint()).UTC()
	e.Method = d.string()
	e.Query = d.string()
	e.Header = http.Header{}
	for range d.count() {
		name := d.string()
		values := make([]string, d.count())
		for i := range values {
			values[i] = d.string()
		}
		e.Header[name] = values
	}
	e.Body = d.bytes()
	switch {
	case d.err != nil:
		return nil, d.err
	case flags&^1 != 0:
		return nil, fmt.Errorf("%w: unknown flags", ErrMalformed)
	case len(d.data) > 0:
		return nil, fmt.Errorf("%w: trailing data", ErrMalformed)
	}
	return e, nil
}

// decoder reads fields; after the first error every read returns a zero
// value.
type decoder struct {
	data []byte
	err  error
}

func (d *decoder) fail() {
	if d.err == nil {
		d.err = fmt.Errorf("%w: truncated", ErrMalformed)
	}
	d.data = nil
}

func (d *decoder) uvarint() uint64 {
	v, n := binary.Uvarint(d.data)
	if n <= 0 {
		d.fail()
		return 0
	}
	d.data = d.data[n:]
	return v
}

func (d *decoder) varint() int64 {
	v, n := binary.Varint(d.data)
	if n <= 0 {
		d.fail()
		return 0
	}
	d.data = d.data[n:]
	return v
}

func (d *decoder) byte() byte {
	if len(d.data) < 1 {
		d.fail()
		return 0
	}
	c := d.data[0]
	d.data = d.data[1:]
	return c
}

// count reads a number of items. Every item takes at least one byte, so a
// count larger than the remaining data is malformed; this bounds allocations.
func (d *decoder) count() int {
	n := d.uvarint()
	if n > uint64(len(d.data)) {
		d.fail()
		return 0
	}
	return int(n)
}

func (d *decoder) bytes() []byte {
	n := d.count()
	b := d.data[:n:n]
	d.data = d.data[n:]
	return b
}

func (d *decoder) string() string {
	return string(d.bytes())
}
