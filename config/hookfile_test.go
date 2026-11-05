package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Verifies 8a79b4f5c699: the hook-file predicate the run preflight and the
// live view share names the property a script lacks — presence, a regular
// file, the executable bit, the interpreter line — and accepts a script
// that has all four.
func TestCheckHookFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	os.Mkdir(filepath.Join(dir, "adir"), 0o755)
	cases := map[string]string{
		filepath.Join(dir, "absent"):                "not found",
		filepath.Join(dir, "adir"):                  "a directory, not a script",
		write("noexec", "#!/bin/sh\n", 0o644):       "not executable",
		write("nobang", "echo hi\n", 0o755):         "no #! interpreter line",
		write("short", "#", 0o755):                  "no #! interpreter line",
		write("good", "#!/bin/sh\nexit 0\n", 0o755): "",
	}
	for path, want := range cases {
		err := CheckHookFile(path)
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: unexpected %v", filepath.Base(path), err)
		case want != "" && (err == nil || err.Error() != want):
			t.Errorf("%s: got %v, want %q", filepath.Base(path), err, want)
		}
	}
}
