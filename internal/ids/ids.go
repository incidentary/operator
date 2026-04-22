// Package ids generates canonical UUIDv4 identifiers for operator-emitted
// Incidentary events.
//
// UUIDv4 (RFC 9562 §5.4) is 122 bits of CSPRNG random with no embedded
// timestamp. The server accepts v1/v4/v7 transparently — the binary
// representation is identical across versions — but every Incidentary
// producer (server, SDKs, operator) emits v4 as of 2026-04-22.
//
// Earlier drafts of this package emitted UUIDv7 on the grounds that the
// 48-bit millisecond prefix would improve ClickHouse sort-key locality
// on hot ingest paths. That reasoning was wrong for the Incidentary
// server schema:
//
//   - ClickHouse compares UUIDs second-half-first for historical
//     reasons, so v7's timestamp prefix contributes nothing to sparse
//     primary-index ordering or pruning.
//   - Every UUID-bearing ClickHouse table already carries time locality
//     in an explicit i64 nanosecond column (wall_ts_ns / occurred_at)
//     that sits *before* the UUID in the sort key.
//
// With the storage-locality case empty, the remaining consideration is
// v7's 48-bit timestamp prefix — a recoverable creation-time side
// channel for any value that might cross a trust boundary. v4 has no
// such leak.
//
// Randomness comes from github.com/google/uuid's default NewString(),
// which wraps crypto/rand (OS CSPRNG).
package ids

import "github.com/google/uuid"

// NewID returns a canonical UUIDv4 string (RFC 9562 §5.4): 36 chars,
// four hyphens, version nibble '4', RFC 4122 variant bits.
//
// uuid.NewString() panics on a crypto/rand failure, which matches the
// rest of the Go ecosystem's behavior for CSPRNG exhaustion and is the
// correct signal for an operator that cannot safely continue emitting
// ids.
func NewID() string {
	return uuid.NewString()
}
