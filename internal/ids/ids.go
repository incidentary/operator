// Package ids generates canonical UUIDv7 identifiers for operator-emitted
// Incidentary events.
//
// UUIDv7 (RFC 9562 §5.7) encodes a Unix-millis timestamp in the most
// significant 48 bits. Ids generated minutes apart sort lexicographically
// in the order they were created — a material speedup for ClickHouse
// sort-key locality on hot ingest paths. Binary-compatible with v4 on
// the wire, so every place the operator previously emitted
// `uuid.NewString()` can switch to `ids.NewID()` transparently.
package ids

import "github.com/google/uuid"

// NewID returns a canonical UUIDv7 string.
//
// Falls back to a random (v4) id on an exceedingly rare NewV7 failure
// rather than panicking — a v4 string is still valid on the wire, so
// it is strictly better than crashing an operator tight loop. Producers
// emitting a mix of v4 and v7 remain interoperable per wire-format
// v2 §7.1.
func NewID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}
