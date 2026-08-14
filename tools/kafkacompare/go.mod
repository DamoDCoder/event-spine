// The Kafka comparison lives in its own module so the spine has none of its
// dependencies. A project adopting the log should not inherit a Kafka client to
// get one, and docs/decisions/m5-comparison-protocol.md is the only reason this
// client exists at all.
module github.com/DamoDCoder/event-spine/tools/kafkacompare

go 1.25.0

require (
	github.com/DamoDCoder/event-spine v0.0.0
	github.com/twmb/franz-go v1.21.0
	github.com/twmb/franz-go/pkg/kadm v1.18.0
)

require (
	github.com/klauspost/compress v1.18.5 // indirect
	github.com/pierrec/lz4/v4 v4.1.26 // indirect
	github.com/twmb/franz-go/pkg/kmsg v1.13.1 // indirect
	golang.org/x/crypto v0.50.0 // indirect
)

replace github.com/DamoDCoder/event-spine => ../..
