package compare

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/DamoDCoder/event-spine/internal/core"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Kafka is Apache Kafka behind the comparison interface.
//
// The durability pairing is the one declared in
// docs/decisions/m5-comparison-protocol.md: acks decides what the producer
// waits for, and the topic's flush.messages decides what the broker writes
// through. Both were fixed before any number was taken.
type Kafka struct {
	client *kgo.Client
	admin  *kadm.Client
	topic  string
	broker string
}

// KafkaTimeout bounds every call. A comparison that hangs is a comparison
// nobody finishes, and a broker that stopped answering is a result rather than
// a reason to wait.
const KafkaTimeout = 2 * time.Minute

// OpenKafka creates a fresh topic configured for the mode and connects to it.
//
// A fresh topic per run, because appending to one that already holds a previous
// run's records would measure a log of a different size.
func OpenKafka(broker string, mode Mode, topic string) (*Kafka, error) {
	admin, err := kadm.NewOptClient(kgo.SeedBrokers(broker))
	if err != nil {
		return nil, fmt.Errorf("compare: kafka admin: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), KafkaTimeout)
	defer cancel()

	// One partition and one replica: the spine has neither partitioning nor
	// replication, and the protocol records that configuring Kafka's away is
	// what makes the comparison possible while removing its main advantage.
	configs := map[string]*string{
		"flush.messages": flushMessages(mode),

		// Time-based flushing is disabled so the record count is the
		// only thing that triggers a write-through. Leaving it at the
		// default would make the flush interval depend on how long the
		// run happened to take.
		"flush.ms": ptr(strconv.FormatInt(int64(^uint32(0)>>1), 10)),

		// Nothing may be deleted or compacted underneath the run.
		"retention.ms":     ptr("-1"),
		"cleanup.policy":   ptr("delete"),
		"compression.type": ptr("uncompressed"),
	}
	if _, err := admin.CreateTopic(ctx, 1, 1, configs, topic); err != nil {
		admin.Close()
		return nil, fmt.Errorf("compare: create topic %s: %w", topic, err)
	}

	opts := []kgo.Opt{
		kgo.SeedBrokers(broker),
		kgo.DefaultProduceTopic(topic),

		// The producer must not compress: the spine does not, and a
		// comparison where one side ships fewer bytes is a comparison
		// of compression.
		kgo.ProducerBatchCompression(kgo.NoCompression()),
	}

	switch mode {
	case Sync, Batch:
		opts = append(opts, kgo.RequiredAcks(kgo.AllISRAcks()))
	case OS:
		// Leader acknowledgement only, which is the pairing for a spine
		// mode that never forces a flush. Idempotent writes require
		// acks=all, so they go with it.
		opts = append(opts, kgo.RequiredAcks(kgo.LeaderAck()), kgo.DisableIdempotentWrite())
	default:
		admin.Close()
		return nil, fmt.Errorf("compare: unknown mode %q", mode)
	}

	client, err := kgo.NewClient(opts...)
	if err != nil {
		admin.Close()
		return nil, fmt.Errorf("compare: kafka client: %w", err)
	}
	if err := client.Ping(ctx); err != nil {
		client.Close()
		admin.Close()
		return nil, fmt.Errorf("compare: ping %s: %w", broker, err)
	}

	return &Kafka{client: client, admin: admin, topic: topic, broker: broker}, nil
}

// flushMessages is the broker-side half of the durability pairing.
func flushMessages(mode Mode) *string {
	switch mode {
	case Sync:
		return ptr("1")
	case Batch:
		return ptr(strconv.Itoa(FlushEvery))
	default:
		// Kafka's default: never triggered by count, left to the
		// operating system, which is the pairing for the spine's os
		// mode.
		return ptr(strconv.FormatInt(int64(^uint64(0)>>1), 10))
	}
}

func ptr(s string) *string { return &s }

func (k *Kafka) Name() string { return "kafka" }

// Append produces the batch and waits for every record to be acknowledged.
//
// The wait is the point: an asynchronous produce would measure how fast records
// can be handed to a buffer, which is not what the spine's Append reports.
func (k *Kafka) Append(events []core.Event) error {
	ctx, cancel := context.WithTimeout(context.Background(), KafkaTimeout)
	defer cancel()

	records := make([]*kgo.Record, len(events))
	for i, e := range events {
		// The value is the spine's own canonical record body, so both
		// systems store byte-identical user data and each pays for its
		// own framing.
		records[i] = &kgo.Record{Value: e.AppendCanonical(nil)}
	}

	return k.client.ProduceSync(ctx, records...).FirstErr()
}

// ReadAll consumes from the beginning of the topic, through a consumer created
// here rather than at connect time.
//
// The first version of this shared one client with the producer, and a client
// told to consume starts fetching immediately — so records arrived in its buffer
// while the appends were still running, and the slower the append mode the more
// of the read had already happened before the read was timed. It reported
// 1.9M records/sec in the mode with the slowest writes and 29k in the fastest,
// which is the shape of a measurement of prefetching rather than of reading.
func (k *Kafka) ReadAll(want int) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), KafkaTimeout)
	defer cancel()

	consumer, err := kgo.NewClient(
		kgo.SeedBrokers(k.broker),
		kgo.ConsumeTopics(k.topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().At(0)),
	)
	if err != nil {
		return 0, fmt.Errorf("compare: kafka consumer: %w", err)
	}
	defer consumer.Close()

	var seen int
	for seen < want {
		fetches := consumer.PollRecords(ctx, want-seen)
		if err := fetches.Err(); err != nil {
			return seen, err
		}
		if fetches.NumRecords() == 0 {
			return seen, fmt.Errorf("compare: kafka returned no records with %d still wanted", want-seen)
		}
		seen += fetches.NumRecords()
	}
	return seen, nil
}

// Close deletes the topic as well as disconnecting, so a second run does not
// inherit the first one's data.
func (k *Kafka) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), KafkaTimeout)
	defer cancel()

	_, err := k.admin.DeleteTopics(ctx, k.topic)
	k.client.Close()
	k.admin.Close()
	if err != nil {
		return fmt.Errorf("compare: delete topic %s: %w", k.topic, err)
	}
	return nil
}
