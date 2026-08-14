package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/agent/contract"
	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
)

// sessionProfile is a resolved profile with the session dialect set.
func sessionProfile() *config.ResolvedInvoke {
	ri := config.DefaultInvoke()
	ri.SessionDir = ".h/sessions"
	ri.ResumeArgv = []string{"harness", "resume", "--latest"}
	return &ri
}

// Verifies spec/proposals/2026-08-14-session-transcripts.md (pipeline test 16):
// capture gates on the per-run toggle AND the profile's session_dir; on, the
// attempt gets a fresh empty host dir mounted read-write at
// $HOME/<session_dir> with FABER_SESSIONS_DIR set; each attempt's dir is its
// own, and earlier attempts' transcripts survive later attempts' scrubs.
func TestBoxRun_SessionsCapture(t *testing.T) {
	containers := &fakeContainers{record: &contract.Result{Status: contract.StatusOK, Payload: map[string]any{"out": "d"}, Attempt: 1}}
	boxes := &AgentBoxes{Containers: containers, Bindings: &fakeBindings{}, EntryBinary: "/usr/local/bin/faber-box"}

	box := boxAttempt(t)
	box.Sessions = true
	box.Template.Invoke = sessionProfile()
	if _, err := boxes.RunAttempt(context.Background(), box); err != nil {
		t.Fatalf("run attempt: %v", err)
	}
	spec := containers.specs[0]
	wantContainer := contract.ContainerHome + "/.h/sessions"
	wantHost := filepath.Join(box.RunDir, "boxes", pathToken(box.NodeID), "attempt-1", "sessions")
	if got := spec.Env[contract.EnvSessionsDir]; got != wantContainer {
		t.Errorf("env[%s] = %q, want %q", contract.EnvSessionsDir, got, wantContainer)
	}
	found := false
	for _, m := range spec.Mounts {
		if m.Container == wantContainer {
			found = true
			if m.Host != wantHost || m.ReadOnly {
				t.Errorf("sessions mount = %+v, want %s bound read-write", m, wantHost)
			}
		}
	}
	if !found {
		t.Fatalf("sessions bind missing from mounts: %v", spec.Mounts)
	}
	if fi, err := os.Stat(wantHost); err != nil || !fi.IsDir() {
		t.Fatalf("sessions host dir must exist before the container runs: %v", err)
	}

	// A transcript the harness "wrote" in attempt 1 survives attempt 2's
	// scrub (the scrub clears only its own attempt dir) — and attempt 2 gets
	// its own fresh dir.
	if err := os.WriteFile(filepath.Join(wantHost, "t.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	box.Attempt = 2
	if _, err := boxes.RunAttempt(context.Background(), box); err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wantHost, "t.jsonl")); err != nil {
		t.Fatalf("attempt 1's transcript must survive attempt 2: %v", err)
	}
	host2 := filepath.Join(box.RunDir, "boxes", pathToken(box.NodeID), "attempt-2", "sessions")
	if entries, err := os.ReadDir(host2); err != nil || len(entries) != 0 {
		t.Fatalf("attempt 2's sessions dir must exist empty, got %v (%v)", entries, err)
	}

	// The resume-reuse case: a re-run at the SAME attempt number (resumed
	// runs restart attempt numbering) must preserve the prior execution's
	// transcript beside the attempt dir before the scrub, and start fresh.
	if err := os.WriteFile(filepath.Join(host2, "t2.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := boxes.RunAttempt(context.Background(), box); err != nil {
		t.Fatalf("re-run at attempt 2: %v", err)
	}
	preserved := filepath.Join(box.RunDir, "boxes", pathToken(box.NodeID), "attempt-2.sessions.1")
	if _, err := os.Stat(filepath.Join(preserved, "t2.jsonl")); err != nil {
		t.Fatalf("prior transcript must be preserved at %s: %v", preserved, err)
	}
	if entries, err := os.ReadDir(host2); err != nil || len(entries) != 0 {
		t.Fatalf("the re-run must start with a fresh empty sessions dir, got %v (%v)", entries, err)
	}
}

// Verifies pipeline test 16's off-gates: sessions off, or a profile without
// session_dir, or no profile — no bind, no variable, run specs byte-identical
// to a capture-less attempt.
func TestBoxRun_SessionsOffGates(t *testing.T) {
	run := func(t *testing.T, mutate func(*BoxAttempt)) {
		t.Helper()
		containers := &fakeContainers{record: &contract.Result{Status: contract.StatusOK, Payload: map[string]any{"out": "d"}, Attempt: 1}}
		boxes := &AgentBoxes{Containers: containers, Bindings: &fakeBindings{}, EntryBinary: "/usr/local/bin/faber-box"}
		box := boxAttempt(t)
		mutate(&box)
		if _, err := boxes.RunAttempt(context.Background(), box); err != nil {
			t.Fatalf("run attempt: %v", err)
		}
		spec := containers.specs[0]
		if v, ok := spec.Env[contract.EnvSessionsDir]; ok {
			t.Errorf("env[%s] = %q, want absent", contract.EnvSessionsDir, v)
		}
		for _, m := range spec.Mounts {
			if strings.HasPrefix(m.Container, contract.ContainerHome+"/") {
				t.Errorf("unexpected mount inside HOME: %+v", m)
			}
		}
		if _, err := os.Stat(filepath.Join(box.RunDir, "boxes", pathToken(box.NodeID), "attempt-1", "sessions")); !os.IsNotExist(err) {
			t.Errorf("no sessions host dir may be created, stat err %v", err)
		}
	}
	t.Run("toggle off despite a session_dir profile", func(t *testing.T) {
		run(t, func(b *BoxAttempt) { b.Template.Invoke = sessionProfile() })
	})
	t.Run("toggle on without a session_dir profile", func(t *testing.T) {
		run(t, func(b *BoxAttempt) {
			b.Sessions = true
			ri := config.DefaultInvoke()
			b.Template.Invoke = &ri
		})
	})
	t.Run("toggle on without any profile", func(t *testing.T) {
		run(t, func(b *BoxAttempt) { b.Sessions = true })
	})
}

// Verifies pipeline test 17: interactive re-entry lands the operator inside
// the harness's resumed session when the failed attempt saved one — the
// profile's resume_argv as entry, HOME pinned to the box home, and the mount
// at $HOME/<session_dir> a COPY under the salted throwaway dir, so the
// archived attempt bytes stay immutable. Each fallback yields the bare shell.
func TestBoxRun_ReentrySessionResume(t *testing.T) {
	setup := func(t *testing.T, tpl *config.ResolvedTemplate, saved map[string]string) (*failure.Store, *fakeInteractive, *Reentry, string) {
		t.Helper()
		store := failure.NewStore(t.TempDir(), nil)
		ir := testIR("w", []config.Node{agentNode("w/x", "out")}, nil)
		ir.Nodes[0].Template = tpl
		hash, _ := config.HashIR(ir)
		seed, err := store.Fresh(failure.Header{RunID: "run-s", Workflow: "w", IRHash: hash, Started: testBase, Sessions: true})
		if err != nil {
			t.Fatalf("fresh: %v", err)
		}
		handoffRel := filepath.Join("boxes", pathToken("w/x"), "attempt-1", "result")
		handoffDir := filepath.Join(store.RunDir("run-s"), handoffRel)
		if err := os.MkdirAll(handoffDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := contract.WriteHandoffFile(handoffDir, contract.Handoff{
			Keying: contract.HandoffKeyingSlot,
			Phase:  "agent", Reason: "agent-failed",
			Inputs: map[string]string{"out": "v"},
		}); err != nil {
			t.Fatal(err)
		}
		if err := seed.Journal.AppendResult(failure.ResultRecord{StepID: "w/x", InputHash: "h", Result: failure.Result{
			Status:  failure.StatusFailed,
			Error:   &failure.ErrorRecord{Reason: "agent-failed", Detail: "died", Handoff: filepath.Join(handoffRel, contract.HandoffFile)},
			Attempt: 1,
		}}); err != nil {
			t.Fatal(err)
		}
		seed.Journal.Close()

		savedDir := filepath.Join(store.RunDir("run-s"), "boxes", pathToken("w/x"), "attempt-1", "sessions")
		for name, content := range saved {
			if err := os.MkdirAll(savedDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(savedDir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		interactive := &fakeInteractive{}
		re := &Reentry{
			IR:          ir,
			Images:      fakeTags{},
			Bindings:    &fakeBindings{},
			Interactive: interactive,
			EntryBinary: "/usr/local/bin/faber-box",
		}
		return store, interactive, re, savedDir
	}
	sessionTemplate := func() *config.ResolvedTemplate {
		tpl := testTemplate("worker", "out")
		tpl.Env = map[string]string{contract.EnvAgentCLI: "agent-cli"}
		tpl.Inputs = map[string]config.ParamDef{"out": {Type: "string", Required: true}}
		tpl.Invoke = sessionProfile()
		return tpl
	}

	t.Run("saved session resumes", func(t *testing.T) {
		store, interactive, re, savedDir := setup(t, sessionTemplate(), map[string]string{"t.jsonl": "{}\n"})
		copied := ""
		interactive.onRun = func() {
			// Captured DURING the session: the salted dir (and the copy) are
			// removed when Reenter returns.
			for _, m := range interactive.spec.Mounts {
				if m.Container == contract.ContainerHome+"/.h/sessions" {
					copied = m.Host
				}
			}
			if copied == "" {
				t.Errorf("session copy not mounted: %v", interactive.spec.Mounts)
				return
			}
			if copied == savedDir {
				t.Errorf("the archive itself is mounted; want a copy")
			}
			if raw, err := os.ReadFile(filepath.Join(copied, "t.jsonl")); err != nil || string(raw) != "{}\n" {
				t.Errorf("copy misses the transcript: %q %v", raw, err)
			}
			// Diverge the ephemeral session; the archive must stay untouched.
			os.WriteFile(filepath.Join(copied, "t.jsonl"), []byte("diverged"), 0o644)
		}
		if err := store.Interactive(context.Background(), "run-s", "w/x", false, re); err != nil {
			t.Fatalf("interactive: %v", err)
		}
		spec := interactive.spec
		if got := strings.Join(spec.Entry, " "); got != "harness resume --latest" {
			t.Fatalf("entry = %v, want the profile resume_argv", spec.Entry)
		}
		if spec.Env["HOME"] != contract.ContainerHome {
			t.Errorf("HOME = %q, want the box home", spec.Env["HOME"])
		}
		if raw, err := os.ReadFile(filepath.Join(savedDir, "t.jsonl")); err != nil || string(raw) != "{}\n" {
			t.Errorf("archived transcript changed: %q %v — the record must stay immutable", raw, err)
		}
		if copied != "" {
			if _, err := os.Stat(copied); !os.IsNotExist(err) {
				t.Errorf("the session copy must go with the salted dir, stat err %v", err)
			}
		}
	})

	shellCase := func(t *testing.T, tpl *config.ResolvedTemplate, saved map[string]string, shell bool) {
		t.Helper()
		store, interactive, re, _ := setup(t, tpl, saved)
		if err := store.Interactive(context.Background(), "run-s", "w/x", shell, re); err != nil {
			t.Fatalf("interactive: %v", err)
		}
		if got := interactive.spec.Entry[0]; got != "/bin/sh" {
			t.Fatalf("entry = %v, want the bare shell", interactive.spec.Entry)
		}
		for _, m := range interactive.spec.Mounts {
			if strings.HasPrefix(m.Container, contract.ContainerHome+"/") {
				t.Errorf("unexpected session mount on the shell path: %+v", m)
			}
		}
	}
	t.Run("the copy preserves mtimes and symlinks", func(t *testing.T) {
		// Harnesses find "the most recent session" via file mtimes or a
		// latest-pointer symlink; a copy that flattens either resumes the
		// wrong conversation.
		store, interactive, re, savedDir := setup(t, sessionTemplate(), map[string]string{"old.jsonl": "{}\n", "new.jsonl": "{}\n"})
		oldTime := testBase.Add(-2 * 3600e9)
		newTime := testBase
		if err := os.Chtimes(filepath.Join(savedDir, "old.jsonl"), oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(savedDir, "new.jsonl"), newTime, newTime); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("new.jsonl", filepath.Join(savedDir, "latest")); err != nil {
			t.Fatal(err)
		}
		interactive.onRun = func() {
			copied := ""
			for _, m := range interactive.spec.Mounts {
				if m.Container == contract.ContainerHome+"/.h/sessions" {
					copied = m.Host
				}
			}
			if copied == "" {
				t.Error("session copy not mounted")
				return
			}
			if fi, err := os.Stat(filepath.Join(copied, "old.jsonl")); err != nil || !fi.ModTime().Equal(oldTime) {
				t.Errorf("old.jsonl mtime = %v (%v), want %v preserved", fi.ModTime(), err, oldTime)
			}
			if fi, err := os.Stat(filepath.Join(copied, "new.jsonl")); err != nil || !fi.ModTime().Equal(newTime) {
				t.Errorf("new.jsonl mtime = %v (%v), want %v preserved", fi.ModTime(), err, newTime)
			}
			if link, err := os.Readlink(filepath.Join(copied, "latest")); err != nil || link != "new.jsonl" {
				t.Errorf("latest symlink = %q (%v), want new.jsonl recreated verbatim", link, err)
			}
		}
		if err := store.Interactive(context.Background(), "run-s", "w/x", false, re); err != nil {
			t.Fatalf("interactive: %v", err)
		}
	})

	t.Run("--shell forces the shell despite a saved session", func(t *testing.T) {
		shellCase(t, sessionTemplate(), map[string]string{"t.jsonl": "{}\n"}, true)
	})
	t.Run("no saved session falls back to the shell", func(t *testing.T) {
		shellCase(t, sessionTemplate(), nil, false)
	})
	t.Run("no resume_argv falls back to the shell", func(t *testing.T) {
		tpl := sessionTemplate()
		inv := *tpl.Invoke
		inv.ResumeArgv = nil
		tpl.Invoke = &inv
		shellCase(t, tpl, map[string]string{"t.jsonl": "{}\n"}, false)
	})
}

// Verifies pipeline test 16's run-level threading: the scheduler hands the
// effective sessions toggle (journal header OR the invocation's flag) to
// every box attempt, and a fresh run records the flag in its header.
func TestExecutorThreadsSessionsToggle(t *testing.T) {
	ir := testIR("w", []config.Node{agentNode("w/x", "out")}, nil)
	run := func(t *testing.T, h *harness, opts config.RunOptions) bool {
		t.Helper()
		var got bool
		h.boxes.deflt = func(box BoxAttempt) failure.Result {
			got = box.Sessions
			return okPayload(map[string]any{"out": "v"})
		}
		if err := h.run(t, ir, opts); err != nil {
			t.Fatalf("execute: %v", err)
		}
		return got
	}

	t.Run("--sessions on a fresh run reaches the attempt and the header", func(t *testing.T) {
		h := newHarness(t)
		if !run(t, h, config.RunOptions{Sessions: true}) {
			t.Error("--sessions must reach the box attempt")
		}
		rp, err := h.store.Load("run-test")
		if err != nil {
			t.Fatal(err)
		}
		if !rp.Header.Sessions {
			t.Error("the fresh header must record the sessions toggle")
		}
	})
	t.Run("off stays off", func(t *testing.T) {
		if run(t, newHarness(t), config.RunOptions{}) {
			t.Error("sessions off must reach the box attempt as off")
		}
	})
	t.Run("resume --sessions widens a run that started without", func(t *testing.T) {
		// Begin WITHOUT the flag and fail; resume WITH it — the re-run
		// attempt must capture though the header says off (the OR's other
		// arm; deleting `|| opts.Sessions` in the executor fails this).
		h := newHarness(t)
		fail := true
		var resumedSessions bool
		h.boxes.deflt = func(box BoxAttempt) failure.Result {
			if fail {
				fail = false
				return failedResult("agent-failed", "died")
			}
			resumedSessions = box.Sessions
			return okPayload(map[string]any{"out": "v"})
		}
		if err := h.run(t, ir, config.RunOptions{RunID: "run-w"}); err == nil {
			t.Fatal("the seeded failure must fail the run")
		}
		if err := h.run(t, ir, config.RunOptions{Mode: "resume", RunID: "run-w", Sessions: true}); err != nil {
			t.Fatalf("resume: %v", err)
		}
		if !resumedSessions {
			t.Fatal("resume --sessions must reach the re-run attempt")
		}
	})
	t.Run("a header-flagged run captures on plain resume", func(t *testing.T) {
		// Begin with --sessions and a failing step; resume WITHOUT the flag —
		// the re-run attempt must still see capture on (header inheritance).
		h := newHarness(t)
		fail := true
		var resumed, resumedSessions bool
		h.boxes.deflt = func(box BoxAttempt) failure.Result {
			if fail {
				fail = false
				return failedResult("agent-failed", "died")
			}
			resumed, resumedSessions = true, box.Sessions
			return okPayload(map[string]any{"out": "v"})
		}
		if err := h.run(t, ir, config.RunOptions{RunID: "run-r", Sessions: true}); err == nil {
			t.Fatal("the seeded failure must fail the run")
		}
		if err := h.run(t, ir, config.RunOptions{Mode: "resume", RunID: "run-r"}); err != nil {
			t.Fatalf("resume: %v", err)
		}
		if !resumed || !resumedSessions {
			t.Fatalf("resumed attempt ran=%v sessions=%v; plain resume must inherit capture from the header", resumed, resumedSessions)
		}
	})
}
