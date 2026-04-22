package ids

import (
	"regexp"
	"testing"
)

// Canonical UUIDv4 shape: version nibble '4' at index 14, RFC 4122
// variant bits (8/9/a/b) at index 19.
var uuidv4Pattern = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
)

func TestNewIDMatchesV4Pattern(t *testing.T) {
	id := NewID()
	if !uuidv4Pattern.MatchString(id) {
		t.Fatalf("NewID() = %q, does not match v4 pattern", id)
	}
}

func TestNewIDVersionNibbleIsFour(t *testing.T) {
	// The whole point of unifying on v4: a future refactor that
	// silently reinstates v7 must not slip through review. This
	// test screams the moment the version nibble stops being '4'.
	id := NewID()
	if id[14] != '4' {
		t.Errorf("version nibble = %q, want '4'", string(id[14]))
	}
}

func TestNewIDVariantBitsAreRFC4122(t *testing.T) {
	id := NewID()
	c := id[19]
	if c != '8' && c != '9' && c != 'a' && c != 'b' {
		t.Errorf("variant nibble = %q, want 8/9/a/b", string(c))
	}
}

func TestNewIDDoesNotCollideAcrossManySamples(t *testing.T) {
	// v4 has 122 random bits; collision across 4096 samples is
	// effectively zero. A collision here proves the RNG is seeded
	// or deterministic — a fatal bug for an operator that mints
	// deduplication-keyed event ids.
	const n = 4096
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id at iteration %d: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

func TestNewIDIsNotSeriallyOrdered(t *testing.T) {
	// v4 has no embedded timestamp, so two ids generated
	// back-to-back must not have a systematic lexicographic
	// relationship. Guards against a regression that reinstates
	// a time-ordered generator.
	//
	// We sample 500 pairs; a<b and a>b should each land in roughly
	// [150, 350] — well inside a 12σ envelope around 250/500.
	var lt, gt int
	for i := 0; i < 500; i++ {
		a := NewID()
		b := NewID()
		switch {
		case a < b:
			lt++
		case a > b:
			gt++
		default:
			t.Fatalf("impossible collision at %d: %q", i, a)
		}
	}
	if lt < 150 || lt > 350 {
		t.Errorf("a<b happened %d/500 times — ordering is not uniform", lt)
	}
	if gt < 150 || gt > 350 {
		t.Errorf("a>b happened %d/500 times — ordering is not uniform", gt)
	}
}

func TestNewIDIsCanonical36Chars(t *testing.T) {
	id := NewID()
	if len(id) != 36 {
		t.Fatalf("length = %d, want 36", len(id))
	}
	for _, i := range []int{8, 13, 18, 23} {
		if id[i] != '-' {
			t.Errorf("expected '-' at index %d, got %q", i, string(id[i]))
		}
	}
}
