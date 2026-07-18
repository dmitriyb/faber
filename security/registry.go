package security

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"text/tabwriter"
	"unicode"
)

// Entry is one role's record: a key fingerprint plus an optional human label.
// No key material is ever stored — only the public fingerprint the identity
// binding resolves against.
type Entry struct {
	Fingerprint string `json:"fingerprint"`
	Comment     string `json:"comment,omitempty"`
}

// Registry is the whole role→fingerprint table, marshaled as a JSON object
// keyed by role name. It is faber-engine state (roles.json under faber's
// config home), deliberately separate from the user's key material.
type Registry map[string]Entry

// ValidationError marks a bad flag value (malformed fingerprint or role). The
// CLI maps it to the usage exit code; every other registry error is
// operational (exit 1).
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

func newValidationError(format string, a ...any) *ValidationError {
	return &ValidationError{msg: fmt.Sprintf(format, a...)}
}

var (
	// fingerprintRE is the ssh-keygen SHA-256 fingerprint form: "SHA256:" plus
	// 43 unpadded base64 chars.
	fingerprintRE = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)

	// roleRE is a bare identifier — letters, digits, dot, dash, underscore.
	// Rejecting path separators, whitespace, and empties keeps a role name
	// safe as both a JSON key and a template reference, and never a path.
	roleRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// validFingerprint reports whether fp is a well-formed SHA-256 fingerprint.
func validFingerprint(fp string) bool { return fingerprintRE.MatchString(fp) }

// RegistryPath returns roles.json under faber's config home:
// $XDG_CONFIG_HOME/faber/roles.json when XDG_CONFIG_HOME is set and absolute,
// else ~/.config/faber/roles.json.
func RegistryPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "faber", "roles.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// No home and no XDG: fall back to a relative path so behavior is
		// deterministic rather than panicking. In practice one of the two is
		// always set on the hosts faber runs on.
		return filepath.Join(".config", "faber", "roles.json")
	}
	return filepath.Join(home, ".config", "faber", "roles.json")
}

// LoadRegistry reads the registry file. A missing file yields an empty
// registry (not an error): list-keys on a fresh install prints nothing and
// add-key creates the file. A present-but-malformed file is a hard error —
// faber never silently discards a corrupt registry.
func LoadRegistry(path string) (Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Registry{}, nil
		}
		return nil, fmt.Errorf("read role registry %s: %w", path, err)
	}
	reg := Registry{}
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parse role registry %s: %w", path, err)
	}
	return reg, nil
}

// SaveRegistry writes the registry atomically: create the parent dir 0700,
// write the marshaled JSON to a temp file in the same directory, chmod it
// 0600, then rename over the target. Same-dir temp + rename makes the swap
// atomic on one filesystem and never leaves a half-written registry. The JSON
// has sorted keys (encoding/json sorts map keys) and a trailing newline, so
// repeated writes are byte-stable.
func SaveRegistry(path string, reg Registry) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create registry dir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal role registry: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".roles-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create registry temp file: %w", err)
	}
	tmpName := tmp.Name()
	// On any error below, best-effort remove the temp file so a failed write
	// leaves nothing behind.
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("write registry temp file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("chmod registry temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close registry temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("install role registry %s: %w", path, err)
	}
	return nil
}

// AddKey upserts one role→fingerprint entry, returning the mutated registry
// and whether anything actually changed (the caller Saves only when changed).
//
// Idempotency is by content: re-running the identical add-key changes nothing
// and writes nothing. Re-pointing an existing role at a *different*
// fingerprint is refused unless force is set — a silent overwrite would swap a
// box's whole credential out from under a running project. Only the
// fingerprint and label are ever written; no key material.
func AddKey(reg Registry, role, fingerprint, comment string, force bool) (Registry, bool, error) {
	if !roleRE.MatchString(role) {
		return reg, false, newValidationError("invalid role %q: must be a bare identifier (letters, digits, '.', '-', '_'; no path separators or whitespace)", role)
	}
	if !validFingerprint(fingerprint) {
		return reg, false, newValidationError("invalid fingerprint %q: expected SHA256: followed by 43 base64 characters", fingerprint)
	}
	// The comment is a free-form label but it is later printed verbatim through
	// a tabwriter to the operator's terminal (list-keys). Reject control
	// characters so an embedded newline cannot forge or misalign a row and a
	// terminal escape sequence cannot reach the terminal unfiltered.
	for _, r := range comment {
		if unicode.IsControl(r) {
			return reg, false, newValidationError("invalid comment: control characters (newlines, tabs, terminal escapes) are not allowed; use a single printable line")
		}
	}
	if reg == nil {
		reg = Registry{}
	}
	next := Entry{Fingerprint: fingerprint, Comment: comment}
	if cur, ok := reg[role]; ok {
		if cur.Fingerprint != fingerprint && !force {
			return reg, false, fmt.Errorf("role %q already points at %s; refusing to re-point it at %s without --force", role, cur.Fingerprint, fingerprint)
		}
		if cur == next {
			return reg, false, nil // exact no-op
		}
	}
	reg[role] = next
	return reg, true, nil
}

// WriteRegistryList prints every entry, one line per role, in sorted order:
// "<role>  <fingerprint>  <comment>" (the comment column omitted when empty),
// aligned for reading. A missing/empty registry prints a one-line note to
// stderr and nothing to stdout. Fingerprints are public material and safe to
// print; no key material exists to leak.
func WriteRegistryList(reg Registry, stdout, stderr io.Writer) {
	if len(reg) == 0 {
		fmt.Fprintln(stderr, "no roles registered")
		return
	}
	roles := make([]string, 0, len(reg))
	for role := range reg {
		roles = append(roles, role)
	}
	sort.Strings(roles)

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, role := range roles {
		e := reg[role]
		if e.Comment != "" {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", role, e.Fingerprint, e.Comment)
		} else {
			fmt.Fprintf(tw, "%s\t%s\n", role, e.Fingerprint)
		}
	}
	// tabwriter.Flush errors only when the underlying writer does; nothing the
	// caller can do about a broken stdout, so the sole path is best-effort.
	_ = tw.Flush()
}
