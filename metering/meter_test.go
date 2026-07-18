package metering

import (
	"strings"
	"testing"
)

// Verifies a9a5faefadd6: costs sum within the same unit.
func TestCostAddSameUnit(t *testing.T) {
	got, err := (Cost{Unit: "tokens", Amount: 40}).Add(Cost{Unit: "tokens", Amount: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := (Cost{Unit: "tokens", Amount: 42}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// Verifies a9a5faefadd6: cross-unit arithmetic is an error value, never a
// silent coercion — nothing ever sums one unit into another.
func TestCostAddCrossUnitErrors(t *testing.T) {
	_, err := (Cost{Unit: "tokens", Amount: 1}).Add(Cost{Unit: "usd-cents", Amount: 1})
	if err == nil {
		t.Fatal("cross-unit Add succeeded, want error")
	}
	if !strings.Contains(err.Error(), "cross-unit") {
		t.Fatalf("error %q does not name the cross-unit violation", err)
	}
}
