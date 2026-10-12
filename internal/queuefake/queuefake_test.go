package queuefake

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestSendReceiveAck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := New(Options{})
		ctx := t.Context()
		if err := q.Send(ctx, []byte("one")); err != nil {
			t.Fatal(err)
		}
		msgs, err := q.Receive(ctx, 10)
		if err != nil || len(msgs) != 1 {
			t.Fatalf("Receive = %v, %v", msgs, err)
		}
		m := msgs[0]
		if string(m.Body) != "one" || m.DeliveryCount != 1 || m.EnqueuedAt.IsZero() || m.Handle == "" {
			t.Errorf("message = %+v", m)
		}
		if err := q.Ack(ctx, m.Handle); err != nil {
			t.Fatal(err)
		}
		if q.Len() != 0 || q.Sent() != 1 {
			t.Errorf("Len = %d, Sent = %d", q.Len(), q.Sent())
		}
		if err := q.Ack(ctx, m.Handle); err == nil {
			t.Error("second ack succeeded")
		}
	})
}

func TestReceiveWaitsForSend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := New(Options{WaitTime: time.Minute})
		go func() {
			time.Sleep(time.Second)
			q.Send(context.Background(), []byte("late"))
		}()
		msgs, err := q.Receive(t.Context(), 1)
		if err != nil || len(msgs) != 1 || string(msgs[0].Body) != "late" {
			t.Fatalf("Receive = %v, %v", msgs, err)
		}
	})
}

func TestReceiveTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := New(Options{WaitTime: 20 * time.Second})
		start := time.Now()
		msgs, err := q.Receive(t.Context(), 1)
		if err != nil || len(msgs) != 0 || time.Since(start) != 20*time.Second {
			t.Fatalf("Receive = %v, %v after %v", msgs, err, time.Since(start))
		}
	})
}

func TestRedeliveryAndDeadLetter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := New(Options{AckDeadline: 30 * time.Second, MaxDeliveries: 3, WaitTime: time.Hour})
		ctx := t.Context()
		q.Send(ctx, []byte("unlucky"))
		var handles []string
		for want := 1; want <= 3; want++ {
			start := time.Now()
			msgs, err := q.Receive(ctx, 1)
			if err != nil || len(msgs) != 1 || msgs[0].DeliveryCount != want {
				t.Fatalf("delivery %d: Receive = %+v, %v", want, msgs, err)
			}
			if want > 1 && time.Since(start) != 30*time.Second {
				t.Errorf("delivery %d after %v, want the ack deadline", want, time.Since(start))
			}
			handles = append(handles, msgs[0].Handle)
		}
		if err := q.Ack(ctx, handles[0]); err == nil {
			t.Error("ack with a handle of an earlier delivery succeeded")
		}

		short, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		msgs, err := q.Receive(short, 1)
		if !errors.Is(err, context.DeadlineExceeded) || len(msgs) != 0 {
			t.Fatalf("Receive after the last delivery = %v, %v", msgs, err)
		}
		if dead := q.DeadLetters(); len(dead) != 1 || string(dead[0]) != "unlucky" || q.Len() != 0 {
			t.Errorf("dead letters = %q, Len = %d", dead, q.Len())
		}
	})
}

func TestReceiveMax(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := New(Options{})
		for i := range 3 {
			q.Send(t.Context(), []byte(strconv.Itoa(i)))
		}
		msgs, _ := q.Receive(t.Context(), 2)
		if len(msgs) != 2 {
			t.Fatalf("got %d messages, want 2", len(msgs))
		}
		msgs, _ = q.Receive(t.Context(), 2)
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want the remaining 1", len(msgs))
		}
	})
}

func TestSendLimitsAndHook(t *testing.T) {
	q := New(Options{MaxMessageSize: 4})
	if err := q.Send(t.Context(), []byte("12345")); err == nil {
		t.Error("oversized message accepted")
	}
	boom := errors.New("boom")
	q.SetSendHook(func(_ context.Context, n int) error {
		if n == 3 {
			return boom
		}
		return nil
	})
	if err := q.Send(t.Context(), []byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := q.Send(t.Context(), []byte("ok")); !errors.Is(err, boom) {
		t.Errorf("third send: %v, want boom", err)
	}
	if q.Sent() != 1 || len(q.Bodies()) != 1 {
		t.Errorf("Sent = %d, Bodies = %q", q.Sent(), q.Bodies())
	}
}

// The fake must only be used by tests: no non-test file imports it.
func TestOnlyTestsImportFake(t *testing.T) {
	const self = "github.com/NiekKeijzer/webhook-relay/internal/queuefake"
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == self {
				t.Errorf("%s imports the fake queue", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
