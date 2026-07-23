package config

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// The fake-server dry-run harness for install.sh's upgrade path. It drives the
// REAL script (a copy of the canonical repo-root install.sh with only the
// SIGNING_PUBKEY line swapped for a throwaway key — the same three-line
// substitution the delivery module's one-time local proof used, minus the URL
// edits, which are now env-overridable) against a local HTTP server shaped like
// GitHub's release endpoints. It exercises resolve → download → verify (both
// binaries) → self-replace of both running binaries → fail-closed → rollback.
// `sh -n` proves only syntax; this proves the logic.

const (
	fakeTag = "v0.1.3"
	fakeVer = "0.1.3"

	faberOld = "#!/bin/sh\necho 'faber OLD binary v0.1.0'\n"
	boxOld   = "#!/bin/sh\necho 'faber-box OLD binary v0.1.0'\n"
	faberNew = "#!/bin/sh\necho 'faber NEW binary v0.1.3'\n"
	boxNew   = "#!/bin/sh\necho 'faber-box NEW binary v0.1.3'\n"
)

// harness bundles a fresh throwaway release, a signed script copy, a served
// fake GitHub, and a pair of "installed" binaries under a temp bin/ directory.
type harness struct {
	t           *testing.T
	scriptPath  string
	faberTarget string
	boxTarget   string
	apiBase     string
	dlBase      string
	mu          sync.RWMutex      // guards files against the httptest handler goroutine
	files       map[string][]byte // URL path -> body; mutable so a subtest can tamper
}

// getFile / setFile mediate every access to files so the test goroutine (which
// may tamper mid-scenario) and the httptest handler goroutine never touch the
// map unsynchronized — the two are ordered only through the OS socket, which
// the race detector cannot see.
func (h *harness) getFile(path string) ([]byte, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	b, ok := h.files[path]
	return b, ok
}

func (h *harness) setFile(path string, body []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.files[path] = body
}

// newHarness builds everything a scenario needs. It skips (rather than fails)
// when the tool chain or platform the script assumes is unavailable, so the
// suite stays green on hosts that cannot run it.
func newHarness(t *testing.T) *harness {
	t.Helper()
	for _, tool := range []string{"sh", "ssh-keygen", "curl", "tar", "cut", "id", "dirname", "mktemp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("install.sh upgrade harness needs %q on PATH: %v", tool, err)
		}
	}
	goos, goarch := scriptPlatform(t)

	dir := t.TempDir()
	keyPath, pubLine := throwawayKey(t, dir)
	scriptPath := signedScriptCopy(t, dir, pubLine)

	faberArchive := fmt.Sprintf("faber_%s_%s_%s.tar.gz", fakeVer, goos, goarch)
	boxArchive := fmt.Sprintf("faber-box_%s_linux_%s.tar.gz", fakeVer, goarch)
	faberGz := tarGz(t, "faber", faberNew)
	boxGz := tarGz(t, "faber-box", boxNew)

	files := map[string][]byte{
		"/releases/latest":                             []byte(fmt.Sprintf(`{"tag_name": "%s"}`, fakeTag)),
		"/dl/" + fakeTag + "/" + faberArchive:          faberGz,
		"/dl/" + fakeTag + "/" + faberArchive + ".sig": sshSign(t, keyPath, faberGz),
		"/dl/" + fakeTag + "/" + boxArchive:            boxGz,
		"/dl/" + fakeTag + "/" + boxArchive + ".sig":   sshSign(t, keyPath, boxGz),
	}

	h := &harness{t: t, scriptPath: scriptPath, files: files}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := h.getFile(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	h.apiBase = srv.URL
	h.dlBase = srv.URL + "/dl"

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	h.faberTarget = filepath.Join(binDir, "faber")
	h.boxTarget = filepath.Join(binDir, "faber-box")
	h.resetTargets()
	return h
}

// resetTargets restores the pair to the OLD state and clears any .new/.bak
// residue, so a scenario starts from a clean install.
func (h *harness) resetTargets() {
	h.t.Helper()
	for _, p := range []string{h.faberTarget, h.boxTarget} {
		writeExec(h.t, p, map[string]string{h.faberTarget: faberOld, h.boxTarget: boxOld}[p])
		staged, _ := filepath.Glob(p + ".new.*")
		for _, s := range staged {
			_ = os.Remove(s)
		}
		_ = os.Remove(p + ".bak")
	}
}

// run executes the signed script copy in the operator-facing flag contract:
// --target/--box-target are always supplied (per harness), the caller passes
// the mode flags (--upgrade/--rollback/--check/--force/--current …). Only the
// test-only origin bases stay env, matching the real invocation.
func (h *harness) run(flags ...string) (string, error) {
	h.t.Helper()
	args := append([]string{h.scriptPath, "--target", h.faberTarget, "--box-target", h.boxTarget}, flags...)
	cmd := exec.Command("sh", args...)
	cmd.Env = append(os.Environ(),
		"FABER_API_BASE="+h.apiBase,
		"FABER_DL_BASE="+h.dlBase,
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstallUpgradeFakeServer(t *testing.T) {
	t.Run("golden upgrade replaces both running binaries, keeps backups", func(t *testing.T) {
		h := newHarness(t)
		out, err := h.run("--upgrade", "--current", "v0.1.0")
		if err != nil {
			t.Fatalf("upgrade failed: %v\n%s", err, out)
		}
		assertContains(t, h.faberTarget, "NEW")
		assertContains(t, h.boxTarget, "NEW")
		assertContains(t, h.faberTarget+".bak", "OLD")
		assertContains(t, h.boxTarget+".bak", "OLD")
	})

	t.Run("rollback restores both from backup", func(t *testing.T) {
		h := newHarness(t)
		if out, err := h.run("--upgrade", "--current", "v0.1.0"); err != nil {
			t.Fatalf("seed upgrade failed: %v\n%s", err, out)
		}
		assertContains(t, h.faberTarget, "NEW") // precondition: upgraded

		out, err := h.run("--rollback")
		if err != nil {
			t.Fatalf("rollback failed: %v\n%s", err, out)
		}
		assertContains(t, h.faberTarget, "OLD")
		assertContains(t, h.boxTarget, "OLD")
	})

	t.Run("fail-closed: a tampered artifact leaves both running binaries untouched", func(t *testing.T) {
		h := newHarness(t)
		// Flip a byte in the served faber archive; its signature no longer
		// matches, so verification must fail before anything is replaced.
		goos, goarch := scriptPlatform(t)
		faberArchive := "/dl/" + fakeTag + "/" + fmt.Sprintf("faber_%s_%s_%s.tar.gz", fakeVer, goos, goarch)
		orig, _ := h.getFile(faberArchive)
		tampered := append([]byte(nil), orig...)
		tampered[len(tampered)/2] ^= 0xff
		h.setFile(faberArchive, tampered)

		out, err := h.run("--upgrade", "--current", "v0.1.0")
		if err == nil {
			t.Fatalf("upgrade succeeded on a tampered artifact; want non-zero exit\n%s", out)
		}
		if !strings.Contains(out, "verification FAILED") {
			t.Errorf("expected a signature-verification failure message:\n%s", out)
		}
		assertContains(t, h.faberTarget, "OLD")
		assertContains(t, h.boxTarget, "OLD")
		assertNoStaging(t, h.faberTarget)
		assertNoStaging(t, h.boxTarget)
		assertAbsent(t, h.faberTarget+".bak")
		assertAbsent(t, h.boxTarget+".bak")
	})

	t.Run("dry-run verifies but changes nothing", func(t *testing.T) {
		h := newHarness(t)
		out, err := h.run("--upgrade", "--check", "--current", "v0.1.0")
		if err != nil {
			t.Fatalf("dry-run failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "dry-run") {
			t.Errorf("expected a dry-run notice:\n%s", out)
		}
		assertContains(t, h.faberTarget, "OLD")
		assertContains(t, h.boxTarget, "OLD")
		assertNoStaging(t, h.faberTarget)
		assertNoStaging(t, h.boxTarget)
		assertAbsent(t, h.faberTarget+".bak")
	})

	t.Run("already up to date exits 0 without touching the pair", func(t *testing.T) {
		h := newHarness(t)
		out, err := h.run("--upgrade", "--current", fakeTag)
		if err != nil {
			t.Fatalf("expected exit 0 when already current: %v\n%s", err, out)
		}
		if !strings.Contains(out, "already at") {
			t.Errorf("expected an already-up-to-date notice:\n%s", out)
		}
		assertContains(t, h.faberTarget, "OLD")
	})

	t.Run("downgrade is refused without force and allowed with it", func(t *testing.T) {
		h := newHarness(t)
		out, err := h.run("--upgrade", "--current", "v0.9.0")
		if err == nil {
			t.Fatalf("downgrade succeeded without --force:\n%s", out)
		}
		if !strings.Contains(out, "refusing to downgrade") {
			t.Errorf("expected a downgrade refusal:\n%s", out)
		}
		assertContains(t, h.faberTarget, "OLD")

		h.resetTargets()
		out, err = h.run("--upgrade", "--current", "v0.9.0", "--force")
		if err != nil {
			t.Fatalf("forced downgrade failed: %v\n%s", err, out)
		}
		assertContains(t, h.faberTarget, "NEW")
		assertContains(t, h.boxTarget, "NEW")
	})
}

// --- helpers ---

// scriptPlatform maps the running host to the (goos, goarch) install.sh derives
// from uname; it skips the archs faber does not ship.
func scriptPlatform(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("install.sh ships linux/darwin only; host is %s", runtime.GOOS)
	}
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return runtime.GOOS, runtime.GOARCH
	default:
		t.Skipf("install.sh ships amd64/arm64 only; host is %s", runtime.GOARCH)
		return "", ""
	}
}

// throwawayKey generates an ed25519 keypair used solely for this test and
// returns the private-key path and the "type base64 comment" public-key line
// to substitute for SIGNING_PUBKEY.
func throwawayKey(t *testing.T, dir string) (string, string) {
	t.Helper()
	keyPath := filepath.Join(dir, "signing_key")
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-C", "faber-upgrade-test", "-f", keyPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen keygen: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	return keyPath, strings.TrimSpace(string(pub))
}

// signedScriptCopy writes a copy of the canonical repo-root install.sh with
// only the SIGNING_PUBKEY line swapped for the throwaway key.
func signedScriptCopy(t *testing.T, dir, pubLine string) string {
	t.Helper()
	orig, err := os.ReadFile("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(orig), "\n")
	swapped := false
	for i, l := range lines {
		if strings.HasPrefix(l, "SIGNING_PUBKEY=") {
			lines[i] = `SIGNING_PUBKEY="` + pubLine + `"`
			swapped = true
			break
		}
	}
	if !swapped {
		t.Fatal("install.sh has no SIGNING_PUBKEY= line to swap")
	}
	path := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// tarGz builds a gzip-compressed tar holding one regular file (name→content),
// matching the release archive layout install.sh extracts by name.
func tarGz(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte(content)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// sshSign produces an SSHSIG over data with the throwaway key using the exact
// invocation install.sh verifies against (ssh-keygen -Y sign -n file).
func sshSign(t *testing.T, keyPath string, data []byte) []byte {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "artifact-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("ssh-keygen", "-Y", "sign", "-n", "file", "-f", keyPath, f.Name()).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen sign: %v\n%s", err, out)
	}
	sig, err := os.ReadFile(f.Name() + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

func writeExec(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func assertContains(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(b), want) {
		t.Errorf("%s: content %q does not contain %q", path, string(b), want)
	}
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Errorf("%s exists but should not", path)
	}
}

// assertNoStaging asserts no PID-suffixed staging file (<target>.new.*) was
// left behind — the upgrade either renamed it into place or cleaned it up.
func assertNoStaging(t *testing.T, target string) {
	t.Helper()
	matches, err := filepath.Glob(target + ".new.*")
	if err != nil {
		t.Fatalf("glob %s.new.*: %v", target, err)
	}
	if len(matches) > 0 {
		t.Errorf("staging residue left behind: %v", matches)
	}
}
