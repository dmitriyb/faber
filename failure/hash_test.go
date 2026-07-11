package failure

import (
	"encoding/json"
	"strings"
	"testing"
)

// goldenInputHash pins the canonical encoding across processes and faber
// builds: identical inputs must hash identically on every run, or resume
// would silently re-run (or worse, wrongly skip) steps. If this constant ever
// needs changing, every existing journal's keys are invalidated — that is a
// breaking change, not a test fixup.
const goldenInputHash = "7639ede954c858d04a606379ef69099d08e0f54c9bc05fd9c49499529efb9789"

func goldenInputs() map[string]any {
	return map[string]any{
		"text":  "value with <html> & unicode ✓",
		"count": 7,
		"flag":  true,
		"nested": map[string]any{
			"z_last":  []any{1, "two", false},
			"a_first": map[string]any{"inner": "v"},
		},
	}
}

// Verifies bff0f92afc29: the input hash is deterministic — stable across
// processes (golden constant), independent of map iteration order, and
// canonical for nested objects and raw JSON fragments.
func TestInputHashDeterministic(t *testing.T) {
	h1, err := InputHash(goldenInputs(), "template-one", "image:tag-1")
	if err != nil {
		t.Fatal(err)
	}
	if h1 != goldenInputHash {
		t.Fatalf("golden hash drifted:\nwant %s\ngot  %s", goldenInputHash, h1)
	}
	for range 50 { // map order shuffles across iterations
		h, err := InputHash(goldenInputs(), "template-one", "image:tag-1")
		if err != nil {
			t.Fatal(err)
		}
		if h != h1 {
			t.Fatalf("hash unstable across calls: %s vs %s", h, h1)
		}
	}

	// A raw JSON fragment (an upstream payload threaded as an input) hashes
	// the same as its decoded form, whatever its original formatting.
	direct := map[string]any{"upstream": map[string]any{"field": "v", "n": json.Number("3")}}
	viaRaw := map[string]any{"upstream": json.RawMessage("{\n  \"n\": 3, \"field\": \"v\"\n}")}
	hd, err := InputHash(direct, "t", "i")
	if err != nil {
		t.Fatal(err)
	}
	hr, err := InputHash(viaRaw, "t", "i")
	if err != nil {
		t.Fatal(err)
	}
	if hd != hr {
		t.Fatalf("raw JSON fragment not canonicalized: %s vs %s", hd, hr)
	}
}

// Verifies bff0f92afc29 and 87f006277d2c: changing one slot value, the
// template identity, or only the image tag each produces a different hash —
// the exact-reuse rule resume relies on — while an unchanged step hashes
// identically.
func TestInputHashSensitivity(t *testing.T) {
	base, err := InputHash(goldenInputs(), "template-one", "image:tag-1")
	if err != nil {
		t.Fatal(err)
	}
	changedSlot := goldenInputs()
	changedSlot["count"] = 8
	variants := []struct {
		name     string
		inputs   map[string]any
		template string
		image    string
	}{
		{"one slot value", changedSlot, "template-one", "image:tag-1"},
		{"template identity", goldenInputs(), "template-two", "image:tag-1"},
		{"image tag only", goldenInputs(), "template-one", "image:tag-2"},
	}
	seen := map[string]string{base: "base"}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			h, err := InputHash(v.inputs, v.template, v.image)
			if err != nil {
				t.Fatal(err)
			}
			if prev, dup := seen[h]; dup {
				t.Fatalf("changing %s collided with %s: %s", v.name, prev, h)
			}
			seen[h] = v.name
		})
	}
}

// Verifies bff0f92afc29: a raw JSON fragment with trailing bytes fails the
// hash loudly instead of hashing as its prefix — malformed upstream payloads
// must never alias a clean input's journal key. Trailing whitespace alone is
// not data and hashes identically to the clean fragment.
func TestInputHashRejectsTrailingData(t *testing.T) {
	if _, err := InputHash(map[string]any{"x": json.RawMessage(`{"a":1}garbage`)}, "t", "i"); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("want trailing-data error, got %v", err)
	}
	if _, err := InputHash(map[string]any{"x": json.RawMessage(`{"a":1}{"b":2}`)}, "t", "i"); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("want trailing-data error for a second value, got %v", err)
	}
	clean, err := InputHash(map[string]any{"x": json.RawMessage(`{"a":1}`)}, "t", "i")
	if err != nil {
		t.Fatal(err)
	}
	spaced, err := InputHash(map[string]any{"x": json.RawMessage("{\"a\":1}\n  ")}, "t", "i")
	if err != nil {
		t.Fatalf("trailing whitespace is not data: %v", err)
	}
	if clean != spaced {
		t.Fatalf("whitespace changed the hash: %s vs %s", clean, spaced)
	}
}
