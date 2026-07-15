package security

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const (
	fpA = "SHA256:abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ"
	fpB = "SHA256:0123456789012345678901234567890123456789012"
)

// Verifies b145ab5182f4 (scenario 9): add-key on a fresh (missing) registry
// creates roles.json with dir 0700, file 0600, one entry; a second identical
// add-key writes nothing; list-keys prints the entry.
func TestRegistryRoundTripAndIdempotency(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "faber", "roles.json")

	reg, err := LoadRegistry(path) // missing file → empty, not an error
	if err != nil {
		t.Fatalf("LoadRegistry on missing file: %v", err)
	}
	if len(reg) != 0 {
		t.Fatalf("missing registry must read empty, got %v", reg)
	}

	reg, changed, err := AddKey(reg, "implementer_work", fpA, "yubikey 5c", false)
	if err != nil || !changed {
		t.Fatalf("first AddKey: changed=%v err=%v", changed, err)
	}
	if err := SaveRegistry(path, reg); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}

	// Dir 0700, file 0600.
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Fatalf("registry dir perm: want 0700, got %o", perm)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("registry file perm: want 0600, got %o", perm)
	}

	// Identical re-run writes nothing (changed=false), so the caller skips Save.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	reg2, _ := LoadRegistry(path)
	_, changed, err = AddKey(reg2, "implementer_work", fpA, "yubikey 5c", false)
	if err != nil {
		t.Fatalf("idempotent AddKey err: %v", err)
	}
	if changed {
		t.Fatal("identical add-key must be a no-op (changed=false)")
	}

	// list-keys prints the entry.
	var stdout, stderr bytes.Buffer
	WriteRegistryList(reg2, &stdout, &stderr)
	if !bytes.Contains(stdout.Bytes(), []byte("implementer_work")) ||
		!bytes.Contains(stdout.Bytes(), []byte(fpA)) ||
		!bytes.Contains(stdout.Bytes(), []byte("yubikey 5c")) {
		t.Fatalf("list-keys output missing fields: %q", stdout.String())
	}

	// The file is byte-stable across a re-save of the same content.
	if err := SaveRegistry(path, reg2); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatalf("marshaled bytes not stable:\n%q\nvs\n%q", before, after)
	}
}

// Verifies b145ab5182f4 (scenario 9): re-pointing a role at a different
// fingerprint is refused unless --force, and the refusal names the role and
// both fingerprints.
func TestRegistryRepointRequiresForce(t *testing.T) {
	reg := Registry{"reviewer": {Fingerprint: fpA}}

	_, changed, err := AddKey(reg, "reviewer", fpB, "", false)
	if err == nil {
		t.Fatal("re-point without --force must be refused")
	}
	if changed {
		t.Fatal("refused re-point must not report a change")
	}
	for _, want := range []string{"reviewer", fpA, fpB, "--force"} {
		if !bytes.Contains([]byte(err.Error()), []byte(want)) {
			t.Fatalf("refusal error %q missing %q", err.Error(), want)
		}
	}
	// A ValidationError this is NOT — it is an operational refusal (exit 1).
	var ve *ValidationError
	if errors.As(err, &ve) {
		t.Fatal("re-point refusal must not be a ValidationError")
	}

	reg, changed, err = AddKey(reg, "reviewer", fpB, "", true)
	if err != nil || !changed {
		t.Fatalf("forced re-point: changed=%v err=%v", changed, err)
	}
	if reg["reviewer"].Fingerprint != fpB {
		t.Fatalf("forced re-point did not overwrite: %v", reg["reviewer"])
	}
}

// Verifies b145ab5182f4 (scenario 9): a comment-only change on the same
// fingerprint is a change (upsert the label), not a no-op.
func TestRegistryCommentUpdateIsAChange(t *testing.T) {
	reg := Registry{"reviewer": {Fingerprint: fpA, Comment: "old"}}
	reg, changed, err := AddKey(reg, "reviewer", fpA, "new", false)
	if err != nil || !changed {
		t.Fatalf("comment update: changed=%v err=%v", changed, err)
	}
	if reg["reviewer"].Comment != "new" {
		t.Fatalf("comment not updated: %v", reg["reviewer"])
	}
}

// Verifies b145ab5182f4 (scenario 9): malformed fingerprints and roles are
// rejected as ValidationErrors before any write.
func TestRegistryValidationRejectsMalformed(t *testing.T) {
	badFingerprints := []string{
		"",
		"abc",
		"SHA256:tooshort",
		"MD5:abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ",
		"SHA256:abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOP!",  // illegal char
		"SHA256:abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQX", // too long (44)
	}
	for _, fp := range badFingerprints {
		_, changed, err := AddKey(Registry{}, "reviewer", fp, "", false)
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("fingerprint %q: want ValidationError, got %v", fp, err)
		}
		if changed {
			t.Fatalf("fingerprint %q: rejected input must not change the registry", fp)
		}
	}

	badRoles := []string{"", "has space", "a/b", "../etc", "a\tb"}
	for _, role := range badRoles {
		_, changed, err := AddKey(Registry{}, role, fpA, "", false)
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("role %q: want ValidationError, got %v", role, err)
		}
		if changed {
			t.Fatalf("role %q: rejected input must not change the registry", role)
		}
	}

	// A comment with a control character (embedded newline forging a row, a
	// tab misaligning columns, or an ANSI escape reaching the terminal) is
	// rejected before any write; a plain printable comment is accepted.
	badComments := []string{"line1\nline2", "col\tcol", "esc\x1b[31mred", "bell\a"}
	for _, c := range badComments {
		_, changed, err := AddKey(Registry{}, "reviewer", fpA, c, false)
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("comment %q: want ValidationError, got %v", c, err)
		}
		if changed {
			t.Fatalf("comment %q: rejected input must not change the registry", c)
		}
	}
	if _, changed, err := AddKey(Registry{}, "reviewer", fpA, "yubikey 5c (work)", false); err != nil || !changed {
		t.Fatalf("printable comment must be accepted: changed=%v err=%v", changed, err)
	}
}

// Verifies b145ab5182f4 (scenario 9): list-keys on a missing registry prints
// the empty note to stderr, nothing to stdout.
func TestRegistryListEmpty(t *testing.T) {
	var stdout, stderr bytes.Buffer
	WriteRegistryList(Registry{}, &stdout, &stderr)
	if stdout.Len() != 0 {
		t.Fatalf("empty registry must print nothing to stdout, got %q", stdout.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("no roles registered")) {
		t.Fatalf("empty registry note missing: %q", stderr.String())
	}
}

// Verifies b145ab5182f4: a present-but-malformed registry file is a hard error
// (faber never silently discards a corrupt registry).
func TestRegistryMalformedFileIsHardError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roles.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRegistry(path); err == nil {
		t.Fatal("malformed registry file must be a hard error")
	}
}

// Verifies b145ab5182f4: SaveRegistry replaces content atomically and leaves no
// temp file behind.
func TestRegistrySaveLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "faber", "roles.json")
	reg := Registry{"reviewer": {Fingerprint: fpA}}
	if err := SaveRegistry(path, reg); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "roles.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("want only roles.json in the dir, got %v", names)
	}
}

// Verifies the RegistryPath contract: XDG_CONFIG_HOME (absolute) wins.
func TestRegistryPathHonorsXDG(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if got, want := RegistryPath(), filepath.Join(xdg, "faber", "roles.json"); got != want {
		t.Fatalf("RegistryPath: want %q, got %q", want, got)
	}
}
