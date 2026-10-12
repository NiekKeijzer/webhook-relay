// Package queue defines the queue contract (§8 of the architecture). It holds
// interfaces only; each backend implements them in its own package.
package queue

import (
	"context"
	"time"
)

// Sender is the receiver's side of the queue.
type Sender interface {
	// Send stores body durably, or fails.
	Send(ctx context.Context, body []byte) error
	// MaxMessageSize is the largest message the backend accepts, in bytes.
	MaxMessageSize() int
}

// Message is one received message.
type Message struct {
	Body          []byte
	DeliveryCount int
	EnqueuedAt    time.Time
	Handle        string // passed to Ack
}

// Receiver is the forwarder's side of the queue.
type Receiver interface {
	// Receive waits until messages are available or the backend's long-poll
	// timeout passes, and returns at most max messages. When the timeout
	// passes it returns no messages and no error.
	Receive(ctx context.Context, max int) ([]Message, error)
	// Ack removes a message permanently.
	Ack(ctx context.Context, handle string) error
	// AckDeadline is the time after which an unacknowledged message is
	// redelivered.
	AckDeadline() time.Duration
}

// Type is a queue backend, selected by name with the environment variable
// RELAY_QUEUE_TYPE.
type Type struct {
	Name string
	// Params names the backend's settings: uppercase letters, digits and
	// underscores. Each is read from the environment variable
	// RELAY_QUEUE_<name>; an unset or empty variable is left out.
	Params []string
	// Credentials names the backend's secrets, with the same rules. Each is
	// read from RELAY_QUEUE_<name>, or from the file named by
	// RELAY_QUEUE_<name>_FILE, and each is required. The receiver and the
	// forwarder are each given their own values.
	Credentials []string
	// Validate checks the backend's params, without credentials, and returns
	// the queue's ack deadline. Both components run it at startup.
	Validate func(params map[string]string) (ackDeadline time.Duration, err error)
	// NewSender opens the queue for the receiver.
	NewSender func(ctx context.Context, params, credentials map[string]string) (Sender, error)
	// NewReceiver opens the queue for the forwarder.
	NewReceiver func(ctx context.Context, params, credentials map[string]string) (Receiver, error)
}
