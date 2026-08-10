package core

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Field limits, taken from the record header in docs/log-design.md so an event
// that core accepts is an event the log can frame. Rejecting an oversized key
// here rather than at the log boundary means the failure names the command that
// caused it instead of a byte offset.
const (
	// MaxKeyLen is bounded by the header's uint16 key-length field.
	MaxKeyLen = 1<<16 - 1

	// headerBytes is the fixed record header: offset, length, CRC, logical
	// timestamp, schema version, and key length.
	headerBytes = 28

	// MaxPayloadLen leaves room for the header and the largest key inside
	// the header's uint32 total-length field.
	MaxPayloadLen = 1<<32 - 1 - headerBytes - MaxKeyLen
)

// ErrInvalidEvent wraps every event validation failure so callers classify on
// the error rather than on message text.
var ErrInvalidEvent = errors.New("invalid event")

// ErrInvalidCommand wraps every command validation failure.
var ErrInvalidCommand = errors.New("invalid command")

// Event is a fact. Events are never rejected and never mutated after append.
//
// The fields mirror the durable record in docs/log-design.md, minus the three
// the log owns: offset, total length, and CRC. There is deliberately no event
// type field, because the record format does not carry one — a domain
// discriminates on its own payload under the schema version that decodes it.
//
// There is also deliberately no wall-clock field. The M0 spike found one on
// Signal Garden's envelope, unpopulated and waiting for the first ingest
// timestamp to arm it (docs/decisions/m0-determinism-spike.md).
type Event struct {
	// Key is the partition key. Ordering is guaranteed within a key and
	// nowhere else.
	Key string

	// Time is logical, stamped by the cycle from the injected clock. A
	// value set by a decider is overwritten.
	Time Time

	// Schema is the payload version. It is per record rather than per
	// segment, so a rolling schema change does not need a migration.
	Schema uint16

	// Payload is opaque to core. It is not copied on construction, so a
	// caller that retains and mutates the slice has broken immutability
	// for everyone downstream.
	Payload []byte
}

// Validate checks the invariants the log and the fold both depend on.
func (e Event) Validate() error {
	if e.Time < 0 {
		return fmt.Errorf("%w: time must not be negative, got %d", ErrInvalidEvent, e.Time)
	}
	if e.Schema == 0 {
		return fmt.Errorf("%w: schema version must be positive", ErrInvalidEvent)
	}
	if len(e.Key) > MaxKeyLen {
		return fmt.Errorf("%w: key is %d bytes, limit is %d", ErrInvalidEvent, len(e.Key), MaxKeyLen)
	}
	if len(e.Payload) > MaxPayloadLen {
		return fmt.Errorf("%w: payload is %d bytes, limit is %d", ErrInvalidEvent, len(e.Payload), MaxPayloadLen)
	}
	return nil
}

// AppendCanonical appends the event's canonical byte encoding to dst.
//
// The encoding is the durable record's body: byte for byte, the region the log
// frames and covers with its CRC, as laid out in docs/log-design.md. Keeping
// one encoding means a projection digest computed in memory and one computed
// from a replayed segment cannot disagree, which would otherwise be a bug that
// only appears after a restart.
//
// Little-endian, matching the record header. The payload carries no length of
// its own: it runs to the end of the encoding, and the key's length prefix is
// what keeps the encoding injective — "ab" with an empty payload and "a" with a
// payload of "b" differ in their key-length field rather than in where the
// caller decided to split them.
func (e Event) AppendCanonical(dst []byte) []byte {
	dst = binary.LittleEndian.AppendUint64(dst, uint64(e.Time))
	dst = binary.LittleEndian.AppendUint16(dst, e.Schema)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(e.Key)))
	dst = append(dst, e.Key...)
	dst = append(dst, e.Payload...)
	return dst
}

// Command is a proposal. It may be rejected, and a rejected command leaves
// state untouched.
type Command struct {
	// Name identifies the command to the decider.
	Name string

	// Key is the partition the command targets. The events a command
	// produces inherit it unless the decider sets its own.
	Key string

	// Payload is opaque to core, as with Event.
	Payload []byte
}

// Validate checks the envelope. Whether the command is *meaningful* is the
// decider's judgement; this only checks what core itself relies on.
func (c Command) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidCommand)
	}
	if len(c.Key) > MaxKeyLen {
		return fmt.Errorf("%w: key is %d bytes, limit is %d", ErrInvalidCommand, len(c.Key), MaxKeyLen)
	}
	return nil
}

func errMissingDep(name string) error {
	return fmt.Errorf("core: dependency %s is nil", name)
}
