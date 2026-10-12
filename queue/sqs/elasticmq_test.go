package sqs

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/NiekKeijzer/webhook-relay/queue"
)

// TestElasticMQ runs the backend against a real SQS API, such as the
// ElasticMQ in compose.yaml. It runs only when RELAY_TEST_SQS_QUEUE_URL names
// a queue like relay-test in local/elasticmq.conf: a visibility timeout of 1
// second, and dead-lettering after 2 deliveries to the queue at the same URL
// with "-dead-letters" appended. The test empties both queues.
func TestElasticMQ(t *testing.T) {
	queueURL := os.Getenv("RELAY_TEST_SQS_QUEUE_URL")
	if queueURL == "" {
		t.Skip("RELAY_TEST_SQS_QUEUE_URL is not set")
	}
	deadURL := queueURL + "-dead-letters"
	ctx := t.Context()
	p := map[string]string{"URL": queueURL, "REGION": "elasticmq", "VISIBILITY_TIMEOUT_SECONDS": "1"}
	c := map[string]string{"ACCESS_KEY_ID": "local", "SECRET_ACCESS_KEY": "local"}
	s, err := parse(p)
	if err != nil {
		t.Fatal(err)
	}
	client := newClient(s, c)
	for _, u := range []string{queueURL, deadURL} {
		if _, err := client.PurgeQueue(ctx, &sqs.PurgeQueueInput{QueueUrl: aws.String(u)}); err != nil {
			t.Fatal(err)
		}
	}
	snd, err := Type.NewSender(ctx, p, c)
	if err != nil {
		t.Fatal(err)
	}
	rcv, err := Type.NewReceiver(ctx, p, c)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte{0, 1, 2, 0xff, 'x'}
	afterVisibility := func() { time.Sleep(1500 * time.Millisecond) }

	// An acknowledged message is gone.
	sentAt := time.Now()
	if err := snd.Send(ctx, body); err != nil {
		t.Fatal(err)
	}
	m := receiveOne(t, rcv)
	if !bytes.Equal(m.Body, body) || m.DeliveryCount != 1 || m.EnqueuedAt.Sub(sentAt).Abs() > time.Minute {
		t.Errorf("message %+v", m)
	}
	if err := rcv.Ack(ctx, m.Handle); err != nil {
		t.Fatal(err)
	}
	afterVisibility()
	if n := visible(t, client, queueURL); n != 0 {
		t.Errorf("%d messages left after the ack, want 0", n)
	}

	// An unacknowledged message is redelivered after the visibility timeout,
	// then dead-lettered.
	if err := snd.Send(ctx, body); err != nil {
		t.Fatal(err)
	}
	for want := 1; want <= 2; want++ {
		if m := receiveOne(t, rcv); m.DeliveryCount != want {
			t.Errorf("delivery count %d, want %d", m.DeliveryCount, want)
		}
		afterVisibility()
	}
	if n := visible(t, client, queueURL); n != 0 {
		t.Errorf("%d messages left after 2 deliveries, want 0", n)
	}
	if n := visible(t, client, deadURL); n != 1 {
		t.Errorf("%d dead-lettered messages, want 1", n)
	}
}

func receiveOne(t *testing.T, r queue.Receiver) queue.Message {
	t.Helper()
	msgs, err := r.Receive(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("received %d messages, want 1", len(msgs))
	}
	return msgs[0]
}

// visible receives from a queue with a short poll and returns how many
// messages it got.
func visible(t *testing.T, client *sqs.Client, queueURL string) int {
	t.Helper()
	out, err := client.ReceiveMessage(t.Context(), &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(queueURL),
		MaxNumberOfMessages: 10,
		WaitTimeSeconds:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return len(out.Messages)
}
