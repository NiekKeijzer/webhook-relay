package sqs

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSQS is an SQS endpoint that speaks the JSON protocol, as far as this
// package uses it.
type fakeSQS struct {
	t        *testing.T
	mu       sync.Mutex
	attrs    map[string]string // queue attributes
	messages []fakeMessage     // returned by the next ReceiveMessage
	sent     []string          // MessageBody of each SendMessage
	deleted  []string          // ReceiptHandle of each DeleteMessage
	requests []fakeRequest
	errCode  string // when set, every call fails with this error code
}

type fakeRequest struct {
	op, auth string
	input    map[string]any
}

type fakeMessage struct {
	body, handle string
	attrs        map[string]string
}

func newFakeSQS(t *testing.T) (*fakeSQS, string) {
	t.Helper()
	f := &fakeSQS{t: t, attrs: map[string]string{
		"VisibilityTimeout": "30",
		"RedrivePolicy":     `{"deadLetterTargetArn":"arn:aws:sqs:test-region:000000000000:relay-dead-letters","maxReceiveCount":5}`,
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL + "/000000000000/relay"
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (f *fakeSQS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	op := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "AmazonSQS.")
	var in map[string]any
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		f.t.Errorf("%s: decoding the request: %v", op, err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, fakeRequest{op: op, auth: r.Header.Get("Authorization"), input: in})
	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	if f.errCode != "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"__type": "com.amazonaws.sqs#" + f.errCode, "message": "fake failure"})
		return
	}
	var out any
	switch op {
	case "GetQueueAttributes":
		attrs := map[string]string{}
		names, _ := in["AttributeNames"].([]any)
		for _, name := range names {
			if v, ok := f.attrs[name.(string)]; ok {
				attrs[name.(string)] = v
			}
		}
		out = map[string]any{"Attributes": attrs}
	case "SendMessage":
		body, _ := in["MessageBody"].(string)
		f.sent = append(f.sent, body)
		out = map[string]string{"MessageId": "sent", "MD5OfMessageBody": md5Hex(body)}
	case "ReceiveMessage":
		msgs := []map[string]any{}
		for _, m := range f.messages {
			msgs = append(msgs, map[string]any{
				"MessageId":     "received",
				"ReceiptHandle": m.handle,
				"Body":          m.body,
				"MD5OfBody":     md5Hex(m.body),
				"Attributes":    m.attrs,
			})
		}
		f.messages = nil
		out = map[string]any{"Messages": msgs}
	case "DeleteMessage":
		handle, _ := in["ReceiptHandle"].(string)
		f.deleted = append(f.deleted, handle)
		out = map[string]any{}
	default:
		f.t.Errorf("unexpected operation %q", op)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	json.NewEncoder(w).Encode(out)
}

func (f *fakeSQS) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ops []string
	for _, r := range f.requests {
		ops = append(ops, r.op)
	}
	return ops
}

func (f *fakeSQS) last() fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

const secretKey = "sensitive secret key"

var creds = map[string]string{"ACCESS_KEY_ID": "AKIDTEST", "SECRET_ACCESS_KEY": secretKey}

func params(queueURL string) map[string]string {
	return map[string]string{"URL": queueURL, "REGION": "test-region", "VISIBILITY_TIMEOUT_SECONDS": "30"}
}

func TestValidate(t *testing.T) {
	const badURL = "sqs: RELAY_QUEUE_URL must be a queue URL such as https://host/path"
	const badTimeout = "sqs: RELAY_QUEUE_VISIBILITY_TIMEOUT_SECONDS must be a whole number of seconds from 1 to 43200"
	const badSize = "sqs: RELAY_QUEUE_MAX_MESSAGE_SIZE must be a whole number of bytes from 1024 to 1048576"
	tests := []struct {
		name   string
		change map[string]string // an empty value removes the param
		want   string
	}{
		{"missing URL", map[string]string{"URL": ""}, "sqs: RELAY_QUEUE_URL is missing"},
		{"relative URL", map[string]string{"URL": "sqs.example/sensitive"}, badURL},
		{"no queue path", map[string]string{"URL": "https://sqs.example/"}, badURL},
		{"other scheme", map[string]string{"URL": "ftp://sqs.example/1/sensitive"}, badURL},
		{"user info", map[string]string{"URL": "https://sensitive@sqs.example/1/relay"}, badURL},
		{"query", map[string]string{"URL": "https://sqs.example/1/relay?sensitive"}, badURL},
		{"missing region", map[string]string{"REGION": ""}, "sqs: RELAY_QUEUE_REGION is missing"},
		{"missing visibility timeout", map[string]string{"VISIBILITY_TIMEOUT_SECONDS": ""}, "sqs: RELAY_QUEUE_VISIBILITY_TIMEOUT_SECONDS is missing"},
		{"zero visibility timeout", map[string]string{"VISIBILITY_TIMEOUT_SECONDS": "0"}, badTimeout},
		{"visibility timeout too large", map[string]string{"VISIBILITY_TIMEOUT_SECONDS": "43201"}, badTimeout},
		{"visibility timeout with unit", map[string]string{"VISIBILITY_TIMEOUT_SECONDS": "30s"}, badTimeout},
		{"max message size too small", map[string]string{"MAX_MESSAGE_SIZE": "1023"}, badSize},
		{"max message size too large", map[string]string{"MAX_MESSAGE_SIZE": "1048577"}, badSize},
		{"max message size with unit", map[string]string{"MAX_MESSAGE_SIZE": "256KiB"}, badSize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := params("https://sqs.example/000000000000/relay")
			for name, v := range tt.change {
				if v == "" {
					delete(p, name)
				} else {
					p[name] = v
				}
			}
			_, err := Type.Validate(p)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("error %v, want %q", err, tt.want)
			}
		})
	}

	d, err := Type.Validate(params("https://sqs.example/000000000000/relay"))
	if err != nil || d != 30*time.Second {
		t.Fatalf("Validate = %v, %v, want 30s", d, err)
	}
}

func TestSender(t *testing.T) {
	f, queueURL := newFakeSQS(t)
	s, err := Type.NewSender(t.Context(), params(queueURL), creds)
	if err != nil {
		t.Fatal(err)
	}
	// The largest message whose base64 encoding fits in the default 256 KiB.
	if got := s.MaxMessageSize(); got != 196608 || base64.StdEncoding.EncodedLen(got) != 256<<10 {
		t.Errorf("MaxMessageSize = %d, want 196608", got)
	}
	body := []byte{0, 1, 2, 0xff, 'x'}
	if err := s.Send(t.Context(), body); err != nil {
		t.Fatal(err)
	}

	// Opening the queue made no call: the receiver needs only permission to
	// send.
	if got, want := f.ops(), []string{"SendMessage"}; !slices.Equal(got, want) {
		t.Fatalf("operations %v, want %v", got, want)
	}
	if got, err := base64.StdEncoding.DecodeString(f.sent[0]); err != nil || !bytes.Equal(got, body) {
		t.Errorf("sent %q, want the base64 of %v", f.sent[0], body)
	}
	for _, r := range f.requests {
		if r.input["QueueUrl"] != queueURL {
			t.Errorf("%s: QueueUrl %v, want %s", r.op, r.input["QueueUrl"], queueURL)
		}
		if !strings.Contains(r.auth, "Credential=AKIDTEST/") || !strings.Contains(r.auth, "/test-region/sqs/aws4_request") {
			t.Errorf("%s: Authorization %q is not signed with the credentials and region", r.op, r.auth)
		}
	}

	p := params(queueURL)
	p["MAX_MESSAGE_SIZE"] = "1048576"
	if s, err := Type.NewSender(t.Context(), p, creds); err != nil || s.MaxMessageSize() != 786432 {
		t.Errorf("with a 1 MiB queue: MaxMessageSize = %v, %v, want 786432", s, err)
	}

	f.errCode = "QueueDoesNotExist"
	err = s.Send(t.Context(), body)
	if err == nil || !strings.Contains(err.Error(), "QueueDoesNotExist") || strings.Contains(err.Error(), secretKey) {
		t.Errorf("Send error %v, want one naming QueueDoesNotExist without the secret key", err)
	}
}

func TestReceiver(t *testing.T) {
	f, queueURL := newFakeSQS(t)
	r, err := Type.NewReceiver(t.Context(), params(queueURL), creds)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.AckDeadline(); got != 30*time.Second {
		t.Errorf("AckDeadline = %v, want 30s", got)
	}
	if got := f.requests[0].input["AttributeNames"]; !slices.Equal(got.([]any), []any{"VisibilityTimeout", "RedrivePolicy"}) {
		t.Errorf("attributes asked for: %v", got)
	}

	body := []byte{0, 1, 2, 0xff, 'x'}
	attrs := map[string]string{"ApproximateReceiveCount": "3", "SentTimestamp": "1760270400123"}
	f.messages = []fakeMessage{
		{body: base64.StdEncoding.EncodeToString(body), handle: "handle-1", attrs: attrs},
		{body: "not base64!", handle: "handle-2", attrs: attrs},
	}
	msgs, err := r.Receive(t.Context(), 25)
	if err != nil {
		t.Fatal(err)
	}
	in := f.last().input
	if in["MaxNumberOfMessages"] != float64(10) || in["WaitTimeSeconds"] != float64(20) {
		t.Errorf("MaxNumberOfMessages %v, WaitTimeSeconds %v, want 10 and 20", in["MaxNumberOfMessages"], in["WaitTimeSeconds"])
	}
	if got := in["AttributeNames"]; !slices.Equal(got.([]any), []any{"ApproximateReceiveCount", "SentTimestamp"}) {
		t.Errorf("message attributes asked for: %v", got)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	m := msgs[0]
	if !bytes.Equal(m.Body, body) || m.DeliveryCount != 3 || !m.EnqueuedAt.Equal(time.UnixMilli(1760270400123)) || m.Handle != "handle-1" {
		t.Errorf("message %+v", m)
	}
	if len(msgs[1].Body) != 0 || msgs[1].Handle != "handle-2" {
		t.Errorf("a body that is not base64 should be passed on empty: %+v", msgs[1])
	}

	if err := r.Ack(t.Context(), "handle-1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.deleted, []string{"handle-1"}) {
		t.Errorf("deleted %v, want handle-1", f.deleted)
	}

	if _, err := r.Receive(t.Context(), 0); err == nil {
		t.Error("Receive with a maximum of 0 succeeded")
	}
}

func TestReceiveMissingAttributes(t *testing.T) {
	for _, missing := range []string{"ApproximateReceiveCount", "SentTimestamp"} {
		t.Run(missing, func(t *testing.T) {
			f, queueURL := newFakeSQS(t)
			r, err := Type.NewReceiver(t.Context(), params(queueURL), creds)
			if err != nil {
				t.Fatal(err)
			}
			attrs := map[string]string{"ApproximateReceiveCount": "1", "SentTimestamp": "1760270400123"}
			delete(attrs, missing)
			f.messages = []fakeMessage{{body: "AA==", handle: "handle-1", attrs: attrs}}
			_, err = r.Receive(t.Context(), 1)
			if want := "sqs: a message has no " + missing; err == nil || err.Error() != want {
				t.Fatalf("error %v, want %q", err, want)
			}
		})
	}
}

func TestNewReceiverErrors(t *testing.T) {
	tests := []struct {
		name    string
		attrs   map[string]string // replaces the fake's attributes; nil keeps them
		errCode string
		want    string
	}{
		{"no visibility timeout", map[string]string{"RedrivePolicy": "{}"}, "", "sqs: the queue reports no VisibilityTimeout"},
		{"other visibility timeout", map[string]string{"VisibilityTimeout": "60", "RedrivePolicy": "{}"}, "",
			"sqs: the queue's visibility timeout is 60 seconds, but RELAY_QUEUE_VISIBILITY_TIMEOUT_SECONDS is 30"},
		{"no dead-letter queue", map[string]string{"VisibilityTimeout": "30"}, "", "sqs: the queue has no dead-letter queue (RedrivePolicy)"},
		{"API error", nil, "AccessDenied", "sqs: reading the queue's attributes: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, queueURL := newFakeSQS(t)
			if tt.attrs != nil {
				f.attrs = maps.Clone(tt.attrs)
			}
			f.errCode = tt.errCode
			_, err := Type.NewReceiver(t.Context(), params(queueURL), creds)
			if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
				t.Fatalf("error %v, want %q", err, tt.want)
			}
			if tt.errCode != "" && !strings.Contains(err.Error(), tt.errCode) {
				t.Errorf("error %q does not name the API error %s", err, tt.errCode)
			}
			for _, s := range []string{secretKey, "/000000000000/relay"} {
				if strings.Contains(err.Error(), s) {
					t.Errorf("error %q contains %q", err, s)
				}
			}
		})
	}
}
