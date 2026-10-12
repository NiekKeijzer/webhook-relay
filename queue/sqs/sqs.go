// Package sqs is a queue backend (§8 of the architecture) for the Amazon SQS
// API, as offered by Amazon SQS, Scaleway Queues and ElasticMQ.
//
// The queue itself sets the ack deadline (its visibility timeout) and does the
// dead-lettering (its redrive policy). Message bodies are base64, because SQS
// accepts only text and envelopes are binary.
//
// Errors never quote the queue URL or the credentials. Errors from the SQS
// client may name the queue's endpoint.
package sqs

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/NiekKeijzer/webhook-relay/queue"
)

// Type is the SQS backend. Its params and credentials are read from
// RELAY_QUEUE_URL, RELAY_QUEUE_REGION, RELAY_QUEUE_VISIBILITY_TIMEOUT_SECONDS,
// RELAY_QUEUE_MAX_MESSAGE_SIZE (optional), RELAY_QUEUE_ACCESS_KEY_ID and
// RELAY_QUEUE_SECRET_ACCESS_KEY.
var Type = queue.Type{
	Name:        "sqs",
	Params:      []string{paramURL, paramRegion, paramVisibilityTimeout, paramMaxMessageSize},
	Credentials: []string{credAccessKeyID, credSecretAccessKey},
	Validate:    validate,
	NewSender:   newSender,
	NewReceiver: newReceiver,
}

const (
	// paramURL is the queue URL. The endpoint is its scheme and host.
	paramURL    = "URL"
	paramRegion = "REGION"
	// paramVisibilityTimeout must equal the queue's visibility timeout. It is
	// a param because Validate returns the ack deadline without credentials.
	paramVisibilityTimeout = "VISIBILITY_TIMEOUT_SECONDS"
	// paramMaxMessageSize is the queue's maximum message size in bytes. It
	// is a param, not read from the queue, because not every SQS API reports
	// it.
	paramMaxMessageSize = "MAX_MESSAGE_SIZE"
	credAccessKeyID     = "ACCESS_KEY_ID"
	credSecretAccessKey = "SECRET_ACCESS_KEY"
	envPrefix           = "RELAY_QUEUE_"
)

const (
	// waitTimeSeconds is the long-poll timeout of one receive, the SQS
	// maximum.
	waitTimeSeconds = 20
	// maxReceive is the most messages one receive returns, the SQS maximum.
	maxReceive = 10
	// maxVisibilityTimeout is the SQS maximum, in seconds.
	maxVisibilityTimeout = 12 * 60 * 60
	// defaultMaxMessageSize is Scaleway Queues' limit and the SQS default
	// before August 2025: 256 KiB.
	defaultMaxMessageSize = 256 << 10
	// The range of the SQS MaximumMessageSize attribute: 1 KiB to 1 MiB.
	minMaxMessageSize, maxMaxMessageSize = 1 << 10, 1 << 20
)

// System attributes of a received message.
const (
	attrReceiveCount  = "ApproximateReceiveCount"
	attrSentTimestamp = "SentTimestamp"
)

// settings are the checked params.
type settings struct {
	url            string
	endpoint       string
	region         string
	ackDeadline    time.Duration
	maxMessageSize int
}

func parse(params map[string]string) (settings, error) {
	var s settings
	raw := params[paramURL]
	if raw == "" {
		return s, missing(paramURL)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.User != nil ||
		strings.Trim(u.Path, "/") == "" || u.RawQuery != "" || u.Fragment != "" {
		return s, fmt.Errorf("sqs: %s%s must be a queue URL such as https://host/path", envPrefix, paramURL)
	}
	s.url, s.endpoint = raw, u.Scheme+"://"+u.Host
	if s.region = params[paramRegion]; s.region == "" {
		return s, missing(paramRegion)
	}
	raw = params[paramVisibilityTimeout]
	if raw == "" {
		return s, missing(paramVisibilityTimeout)
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 1 || seconds > maxVisibilityTimeout {
		return s, fmt.Errorf("sqs: %s%s must be a whole number of seconds from 1 to %d", envPrefix, paramVisibilityTimeout, maxVisibilityTimeout)
	}
	s.ackDeadline = time.Duration(seconds) * time.Second
	s.maxMessageSize = defaultMaxMessageSize
	if raw = params[paramMaxMessageSize]; raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil || size < minMaxMessageSize || size > maxMaxMessageSize {
			return s, fmt.Errorf("sqs: %s%s must be a whole number of bytes from %d to %d", envPrefix, paramMaxMessageSize, minMaxMessageSize, maxMaxMessageSize)
		}
		s.maxMessageSize = size
	}
	return s, nil
}

func missing(param string) error {
	return fmt.Errorf("sqs: %s%s is missing", envPrefix, param)
}

func validate(params map[string]string) (time.Duration, error) {
	s, err := parse(params)
	return s.ackDeadline, err
}

// newClient returns an SQS client that uses only the given settings and
// credentials, never the AWS environment or shared configuration files.
func newClient(s settings, credentials map[string]string) *sqs.Client {
	creds := aws.Credentials{
		AccessKeyID:     credentials[credAccessKeyID],
		SecretAccessKey: credentials[credSecretAccessKey],
		Source:          "relay environment",
	}
	return sqs.New(sqs.Options{
		Region:       s.region,
		BaseEndpoint: aws.String(s.endpoint),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return creds, nil
		}),
	})
}

func queueAttributes(ctx context.Context, c *sqs.Client, queueURL string, names ...types.QueueAttributeName) (map[string]string, error) {
	out, err := c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(queueURL),
		AttributeNames: names,
	})
	if err != nil {
		return nil, fmt.Errorf("sqs: reading the queue's attributes: %w", err)
	}
	return out.Attributes, nil
}

type sender struct {
	client  *sqs.Client
	url     string
	maxSize int
}

// newSender opens the queue for the receiver. It makes no call to the queue,
// so the receiver needs only permission to send.
func newSender(_ context.Context, params, credentials map[string]string) (queue.Sender, error) {
	s, err := parse(params)
	if err != nil {
		return nil, err
	}
	// The largest message whose base64 encoding fits.
	maxSize := base64.StdEncoding.DecodedLen(s.maxMessageSize)
	return &sender{client: newClient(s, credentials), url: s.url, maxSize: maxSize}, nil
}

func (s *sender) Send(ctx context.Context, body []byte) error {
	_, err := s.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(s.url),
		MessageBody: aws.String(base64.StdEncoding.EncodeToString(body)),
	})
	return err
}

func (s *sender) MaxMessageSize() int { return s.maxSize }

type receiver struct {
	client      *sqs.Client
	url         string
	ackDeadline time.Duration
}

// newReceiver opens the queue for the forwarder. It checks that the queue's
// visibility timeout is the configured ack deadline and that the queue has a
// dead-letter queue.
func newReceiver(ctx context.Context, params, credentials map[string]string) (queue.Receiver, error) {
	s, err := parse(params)
	if err != nil {
		return nil, err
	}
	c := newClient(s, credentials)
	attrs, err := queueAttributes(ctx, c, s.url, types.QueueAttributeNameVisibilityTimeout, types.QueueAttributeNameRedrivePolicy)
	if err != nil {
		return nil, err
	}
	seconds, err := strconv.Atoi(attrs[string(types.QueueAttributeNameVisibilityTimeout)])
	switch {
	case err != nil:
		return nil, errors.New("sqs: the queue reports no VisibilityTimeout")
	case time.Duration(seconds)*time.Second != s.ackDeadline:
		return nil, fmt.Errorf("sqs: the queue's visibility timeout is %d seconds, but %s%s is %d",
			seconds, envPrefix, paramVisibilityTimeout, int(s.ackDeadline/time.Second))
	case attrs[string(types.QueueAttributeNameRedrivePolicy)] == "":
		return nil, errors.New("sqs: the queue has no dead-letter queue (RedrivePolicy)")
	}
	return &receiver{client: c, url: s.url, ackDeadline: s.ackDeadline}, nil
}

func (r *receiver) Receive(ctx context.Context, max int) ([]queue.Message, error) {
	if max < 1 {
		return nil, errors.New("sqs: receive needs a maximum of at least 1")
	}
	out, err := r.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(r.url),
		MaxNumberOfMessages: int32(min(max, maxReceive)),
		WaitTimeSeconds:     waitTimeSeconds,
		// MessageSystemAttributeNames replaces AttributeNames, but Scaleway
		// Queues lists only AttributeNames as supported.
		AttributeNames: []types.QueueAttributeName{attrReceiveCount, attrSentTimestamp},
	})
	if err != nil {
		return nil, err
	}
	msgs := make([]queue.Message, 0, len(out.Messages))
	for _, m := range out.Messages {
		count, err := strconv.Atoi(m.Attributes[attrReceiveCount])
		if err != nil {
			return nil, fmt.Errorf("sqs: a message has no %s", attrReceiveCount)
		}
		sent, err := strconv.ParseInt(m.Attributes[attrSentTimestamp], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("sqs: a message has no %s", attrSentTimestamp)
		}
		// A body that is not base64 did not come from a receiver. It is
		// passed on empty, so it fails the signature check and is
		// dead-lettered.
		body, err := base64.StdEncoding.DecodeString(aws.ToString(m.Body))
		if err != nil {
			body = nil
		}
		msgs = append(msgs, queue.Message{
			Body:          body,
			DeliveryCount: count,
			EnqueuedAt:    time.UnixMilli(sent),
			Handle:        aws.ToString(m.ReceiptHandle),
		})
	}
	return msgs, nil
}

func (r *receiver) Ack(ctx context.Context, handle string) error {
	_, err := r.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(r.url),
		ReceiptHandle: aws.String(handle),
	})
	return err
}

func (r *receiver) AckDeadline() time.Duration { return r.ackDeadline }
