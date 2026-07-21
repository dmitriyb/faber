package main

import (
	"bytes"
	"testing"
)

// Verifies printVersion's output shape matches cmd/faber's (name differs,
// format doesn't) and that the unstamped local-build defaults are the same
// dev/none/unknown fallback.
func TestPrintVersion(t *testing.T) {
	var buf bytes.Buffer
	printVersion(&buf)
	want := "faber-box dev (commit none, built unknown)\n"
	if buf.String() != want {
		t.Fatalf("got %q, want %q", buf.String(), want)
	}
}
