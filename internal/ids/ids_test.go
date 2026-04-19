package ids

import (
	"regexp"
	"testing"
	"time"
)

var uuidv7Pattern = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
)

func TestNewIDMatchesV7Pattern(t *testing.T) {
	id := NewID()
	if !uuidv7Pattern.MatchString(id) {
		t.Fatalf("NewID() = %q, does not match v7 pattern", id)
	}
}

func TestNewIDVersionNibbleIsSeven(t *testing.T) {
	id := NewID()
	if id[14] != '7' {
		t.Errorf("version nibble = %q, want '7'", string(id[14]))
	}
}

func TestNewIDVariantBitsAreRFC4122(t *testing.T) {
	id := NewID()
	c := id[19]
	if c != '8' && c != '9' && c != 'a' && c != 'b' {
		t.Errorf("variant nibble = %q, want 8/9/a/b", string(c))
	}
}

func TestNewIDIsTimeOrdered(t *testing.T) {
	a := NewID()
	time.Sleep(2 * time.Millisecond)
	b := NewID()
	if a >= b {
		t.Errorf("expected a < b lexicographically: a=%q b=%q", a, b)
	}
}

func TestNewIDIsUniqueWithinMillisecond(t *testing.T) {
	const n = 256
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id at iteration %d: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}
