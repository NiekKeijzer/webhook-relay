// Package queuefake is an in-memory queue for tests. It implements the queue
// contract (§8 of the architecture), including redelivery after the ack
// deadline and dead-lettering after a maximum number of deliveries.
//
// It is not a queue backend: no queue type refers to it, so a configuration
// cannot select it. Only _test.go files may import it; a test enforces that.
package queuefake

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/NiekKeijzer/webhook-relay/queue"
)

// Options configures a Queue. Zero fields take the defaults below.
type Options struct {
	MaxMessageSize int           // default 256 KiB
	AckDeadline    time.Duration // default 30 seconds
	MaxDeliveries  int           // default 5
	WaitTime       time.Duration // long-poll timeout; default 100 milliseconds
}

// Queue is an in-memory queue. It is safe for concurrent use.
type Queue struct {
	opts Options

	mu       sync.Mutex
	messages []*message
	dead     [][]byte
	sent     int
	sends    int
	handles  int
	sendHook func(ctx context.Context, n int) error
	wake     chan struct{} // closed when a message is sent
}

type message struct {
	body       []byte
	count      int
	enqueuedAt time.Time
	visibleAt  time.Time
	handle     string
}

var (
	_ queue.Sender   = (*Queue)(nil)
	_ queue.Receiver = (*Queue)(nil)
)

// New returns an empty queue.
func New(opts Options) *Queue {
	if opts.MaxMessageSize == 0 {
		opts.MaxMessageSize = 256 << 10
	}
	if opts.AckDeadline == 0 {
		opts.AckDeadline = 30 * time.Second
	}
	if opts.MaxDeliveries == 0 {
		opts.MaxDeliveries = 5
	}
	if opts.WaitTime == 0 {
		opts.WaitTime = 100 * time.Millisecond
	}
	return &Queue{opts: opts, wake: make(chan struct{})}
}

// SetSendHook makes every Send call hook first, with the 1-based number of
// the call. A non-nil error fails that Send.
func (q *Queue) SetSendHook(hook func(ctx context.Context, n int) error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sendHook = hook
}

// Send stores a copy of body.
func (q *Queue) Send(ctx context.Context, body []byte) error {
	q.mu.Lock()
	q.sends++
	n, hook := q.sends, q.sendHook
	q.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, n); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(body) > q.opts.MaxMessageSize {
		return fmt.Errorf("queuefake: message of %d bytes exceeds %d", len(body), q.opts.MaxMessageSize)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	q.messages = append(q.messages, &message{body: slices.Clone(body), enqueuedAt: now, visibleAt: now})
	q.sent++
	close(q.wake)
	q.wake = make(chan struct{})
	return nil
}

// MaxMessageSize returns the configured maximum.
func (q *Queue) MaxMessageSize() int {
	return q.opts.MaxMessageSize
}

// AckDeadline returns the configured ack deadline.
func (q *Queue) AckDeadline() time.Duration {
	return q.opts.AckDeadline
}

// Receive waits up to the wait time for visible messages and returns at most
// max of them. A message received MaxDeliveries times is dead-lettered
// instead of delivered again.
func (q *Queue) Receive(ctx context.Context, max int) ([]queue.Message, error) {
	timeout := time.NewTimer(q.opts.WaitTime)
	defer timeout.Stop()
	for {
		q.mu.Lock()
		msgs, next := q.take(max, time.Now())
		wake := q.wake
		q.mu.Unlock()
		if len(msgs) > 0 {
			return msgs, nil
		}
		var visible <-chan time.Time
		if !next.IsZero() {
			visible = time.After(time.Until(next))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout.C:
			return nil, nil
		case <-wake:
		case <-visible:
		}
	}
}

// take marks up to max visible messages as received. It also returns when
// the next invisible message becomes visible, or zero if none is invisible.
func (q *Queue) take(max int, now time.Time) ([]queue.Message, time.Time) {
	var msgs []queue.Message
	var next time.Time
	kept := q.messages[:0]
	for _, m := range q.messages {
		switch {
		case m.visibleAt.After(now):
			if next.IsZero() || m.visibleAt.Before(next) {
				next = m.visibleAt
			}
		case len(msgs) >= max:
		case m.count >= q.opts.MaxDeliveries:
			q.dead = append(q.dead, m.body)
			continue
		default:
			m.count++
			m.visibleAt = now.Add(q.opts.AckDeadline)
			q.handles++
			m.handle = fmt.Sprintf("h%d", q.handles)
			if next.IsZero() || m.visibleAt.Before(next) {
				next = m.visibleAt
			}
			msgs = append(msgs, queue.Message{
				Body:          slices.Clone(m.body),
				DeliveryCount: m.count,
				EnqueuedAt:    m.enqueuedAt,
				Handle:        m.handle,
			})
		}
		kept = append(kept, m)
	}
	clear(q.messages[len(kept):])
	q.messages = kept
	return msgs, next
}

// Ack removes the message received with handle. A handle from an earlier
// delivery of a message that was delivered again is no longer valid.
func (q *Queue) Ack(ctx context.Context, handle string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	i := slices.IndexFunc(q.messages, func(m *message) bool { return m.handle == handle })
	if handle == "" || i < 0 {
		return errors.New("queuefake: unknown handle")
	}
	q.messages = slices.Delete(q.messages, i, i+1)
	return nil
}

// Len returns the number of messages that are neither acknowledged nor
// dead-lettered.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.messages)
}

// Sent returns the number of messages sent successfully.
func (q *Queue) Sent() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.sent
}

// Bodies returns copies of the bodies of the messages in the queue, in the
// order they were sent.
func (q *Queue) Bodies() [][]byte {
	q.mu.Lock()
	defer q.mu.Unlock()
	var bodies [][]byte
	for _, m := range q.messages {
		bodies = append(bodies, slices.Clone(m.body))
	}
	return bodies
}

// DeadLetters returns copies of the dead-lettered message bodies.
func (q *Queue) DeadLetters() [][]byte {
	q.mu.Lock()
	defer q.mu.Unlock()
	var bodies [][]byte
	for _, b := range q.dead {
		bodies = append(bodies, slices.Clone(b))
	}
	return bodies
}
