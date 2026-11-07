package contract

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recordingHandler keeps every frame (body read in full) and the foreign
// bytes, in arrival order.
type recordingHandler struct {
	frames  []recorded
	foreign []byte
	// leave, when set, makes Frame read nothing: the reader must drain.
	leave bool
	// fail, when set, is returned from Frame for that kind.
	fail map[string]error
}

type recorded struct {
	kind, name string
	mtime      time.Time
	body       []byte
	n          int64
}

func (h *recordingHandler) Frame(kind, name string, mtime time.Time, body io.Reader, n int64) error {
	rec := recorded{kind: kind, name: name, mtime: mtime, n: n}
	if !h.leave {
		rec.body, _ = io.ReadAll(body)
	}
	h.frames = append(h.frames, rec)
	if err := h.fail[kind]; err != nil {
		return err
	}
	return nil
}

func (h *recordingHandler) Foreign(p []byte) error {
	h.foreign = append(h.foreign, p...)
	return nil
}

// Verifies ff8e85704b0a: a stream of every frame kind round-trips through
// the writer and the reader — names, mtimes, lengths and bodies intact, the
// end frame ending the read — and the raw bytes carry the documented header.
func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewFrameWriter(&buf)
	mtime := time.Date(2026, 11, 7, 10, 0, 0, 123, time.UTC)
	if err := w.Event([]byte(`{"ts":"2026-11-07T10:00:00Z","hook":"h","event":"e"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Frame(FrameFile, "result/output.json", mtime, strings.NewReader(`{"out":"v"}`), 11); err != nil {
		t.Fatal(err)
	}
	if err := w.Frame(FrameLink, "sessions/latest", time.Time{}, strings.NewReader("a.jsonl"), 7); err != nil {
		t.Fatal(err)
	}
	if err := w.Frame(FrameFile, "result/empty", time.Time{}, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := w.End(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "\x00faber\x00event\x00hooks/events.jsonl\x000\x00") {
		t.Fatalf("header shape: %q", buf.String()[:40])
	}
	h := &recordingHandler{}
	if err := ReadFrames(bytes.NewReader(append(buf.Bytes(), []byte("trailing garbage after end")...)), h); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(h.frames) != 5 {
		t.Fatalf("frames = %d, want 5", len(h.frames))
	}
	want := []recorded{
		{kind: FrameEvent, name: HooksEventsFile, body: []byte(`{"ts":"2026-11-07T10:00:00Z","hook":"h","event":"e"}`), n: 52},
		{kind: FrameFile, name: "result/output.json", mtime: mtime, body: []byte(`{"out":"v"}`), n: 11},
		{kind: FrameLink, name: "sessions/latest", body: []byte("a.jsonl"), n: 7},
		{kind: FrameFile, name: "result/empty", body: []byte{}, n: 0},
		{kind: FrameEnd, body: []byte{}},
	}
	for i, f := range h.frames {
		g := want[i]
		if f.kind != g.kind || f.name != g.name || !f.mtime.Equal(g.mtime) || string(f.body) != string(g.body) || f.n != g.n {
			t.Errorf("frame %d = %+v, want %+v", i, f, g)
		}
	}
	if len(h.foreign) != 0 {
		t.Errorf("foreign bytes %q after a clean stream (bytes after end are not read)", h.foreign)
	}
}

// Verifies ff8e85704b0a: bytes outside any frame go to the foreign sink and
// the frames around them still parse — including a stray NUL that is not a
// header, and text split across the reader's buffer.
func TestFrameForeignBytes(t *testing.T) {
	var buf bytes.Buffer
	w := NewFrameWriter(&buf)
	buf.WriteString("hello from a toolset binary\n\x00not a header\n")
	w.Frame(FrameFile, "result/a", time.Time{}, strings.NewReader("A"), 1)
	buf.WriteString(strings.Repeat("x", 3*MaxFrameHeaderBytes))
	w.End()
	h := &recordingHandler{}
	if err := ReadFrames(&buf, h); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(h.frames) != 2 || h.frames[0].name != "result/a" || string(h.frames[0].body) != "A" {
		t.Fatalf("frames = %+v", h.frames)
	}
	if want := "hello from a toolset binary\n\x00not a header\n" + strings.Repeat("x", 3*MaxFrameHeaderBytes); string(h.foreign) != want {
		t.Fatalf("foreign = %q (len %d), want len %d", h.foreign[:min(60, len(h.foreign))], len(h.foreign), len(want))
	}
}

// Verifies ff8e85704b0a: the reader drains a body the handler left unread,
// reports a body cut short, a stream without an end frame, a header over
// the bound, an escaping name, an unknown kind and a length over the record
// bound — each as the documented error, and a handler error verbatim.
func TestFrameReaderBounds(t *testing.T) {
	frame := func(kind, name, mtime, n string, body string) string {
		return FrameMagic + kind + "\x00" + name + "\x00" + mtime + "\x00" + n + "\n" + body
	}
	end := frame(FrameEnd, "", "0", "0", "")

	h := &recordingHandler{leave: true}
	if err := ReadFrames(strings.NewReader(frame(FrameFile, "result/a", "0", "3", "abc")+frame(FrameFile, "result/b", "0", "1", "z")+end), h); err != nil {
		t.Fatalf("unread bodies must be drained: %v", err)
	}
	if len(h.frames) != 3 || h.frames[1].name != "result/b" {
		t.Fatalf("frames after draining = %+v", h.frames)
	}

	cases := map[string]struct {
		stream string
		want   error
		text   string
	}{
		"cut short":       {frame(FrameFile, "result/a", "0", "10", "abc"), ErrFrameStream, "3 of 10"},
		"unterminated":    {frame(FrameFile, "result/a", "0", "1", "a"), ErrStreamUnterminated, ""},
		"escaping name":   {frame(FrameFile, "result/../x", "0", "0", "") + end, ErrFrameStream, "contained"},
		"absolute name":   {frame(FrameFile, "/etc/x", "0", "0", "") + end, ErrFrameStream, "contained"},
		"unknown kind":    {frame("blob", "result/a", "0", "0", "") + end, ErrFrameStream, `kind "blob"`},
		"over the bound":  {frame(FrameFile, "result/a", "0", "99999999999", "") + end, ErrFrameStream, "outside"},
		"bad mtime":       {frame(FrameFile, "result/a", "soon", "0", "") + end, ErrFrameStream, "mtime"},
		"end with a name": {frame(FrameEnd, "x", "0", "0", ""), ErrFrameStream, "end frame"},
		"long header":     {FrameMagic + FrameFile + "\x00" + strings.Repeat("n", MaxFrameHeaderBytes) + "\x000\x000\n" + end, ErrFrameStream, "header"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := ReadFrames(strings.NewReader(tc.stream), &recordingHandler{})
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("err = %v, want %v containing %q", err, tc.want, tc.text)
			}
		})
	}

	boom := errors.New("boom")
	h = &recordingHandler{fail: map[string]error{FrameFile: boom}}
	if err := ReadFrames(strings.NewReader(frame(FrameFile, "result/a", "0", "1", "a")+end), h); !errors.Is(err, boom) {
		t.Fatalf("handler error = %v, want it verbatim", err)
	}
}

// Verifies ff8e85704b0a: the writer refuses what the reader would — an
// invalid name, an unknown kind, an oversize body — before writing a byte,
// and pads a body that ends early so the stream stays aligned, reporting it.
func TestFrameWriterRefusals(t *testing.T) {
	var buf bytes.Buffer
	w := NewFrameWriter(&buf)
	for _, bad := range []func() error{
		func() error { return w.Frame(FrameFile, "../x", time.Time{}, nil, 0) },
		func() error { return w.Frame("blob", "result/x", time.Time{}, nil, 0) },
		func() error { return w.Frame(FrameFile, "result/x", time.Time{}, nil, MaxRecordBytes+1) },
		func() error { return w.Frame(FrameEnd, "x", time.Time{}, nil, 0) },
	} {
		if err := bad(); !errors.Is(err, ErrFrameStream) || buf.Len() != 0 {
			t.Fatalf("refusal: err %v, wrote %d bytes", err, buf.Len())
		}
	}
	err := w.Frame(FrameFile, "result/x", time.Time{}, strings.NewReader("ab"), 5)
	if !errors.Is(err, ErrShortBody) {
		t.Fatalf("short body: %v", err)
	}
	w.End()
	h := &recordingHandler{}
	if rerr := ReadFrames(&buf, h); rerr != nil {
		t.Fatalf("a padded stream must still parse: %v", rerr)
	}
	if string(h.frames[0].body) != "ab\x00\x00\x00" {
		t.Fatalf("padded body = %q", h.frames[0].body)
	}
}

// Verifies 93ba0858d75f: Tree sends a directory's files with their mtimes
// and its symlinks as links, in a stable order, the named last entry last,
// skipping what cannot be sent and a missing root altogether; and the file
// and link writers rebuild the tree with the mtimes and links intact, a
// file arriving through a temporary name.
func TestFrameTreeRoundTrip(t *testing.T) {
	src := t.TempDir()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(rel, content string, mtime time.Time) {
		p := filepath.Join(src, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	mk("result.json", `{"status":"ok"}`, old.Add(time.Hour))
	mk("hooks/events.jsonl", "{}\n", old)
	mk("a.txt", "A", old)
	if err := os.Symlink("a.txt", filepath.Join(src, "latest")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w := NewFrameWriter(&buf)
	var skipped []string
	if err := w.Tree(src, "result", ResultFile, true, func(p string, err error) { skipped = append(skipped, p) }); err != nil {
		t.Fatal(err)
	}
	if err := w.Tree(filepath.Join(src, "missing"), "sessions", "", true, nil); err != nil {
		t.Fatalf("a missing root must send nothing, not fail: %v", err)
	}
	w.End()
	if len(skipped) != 0 {
		t.Fatalf("skipped %v", skipped)
	}
	h := &recordingHandler{}
	if err := ReadFrames(&buf, h); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range h.frames {
		names = append(names, f.kind+" "+f.name)
	}
	want := "file result/a.txt|file result/hooks/events.jsonl|link result/latest|file result/result.json|end "
	if got := strings.Join(names, "|"); got != want {
		t.Fatalf("order = %s\nwant    %s", got, want)
	}
	if !h.frames[0].mtime.Equal(old) || !h.frames[3].mtime.Equal(old.Add(time.Hour)) {
		t.Fatalf("mtimes lost: %v %v", h.frames[0].mtime, h.frames[3].mtime)
	}
	// Without links, the symlink is skipped and reported, the rest sent.
	var buf2 bytes.Buffer
	var noLinkSkips []string
	if err := NewFrameWriter(&buf2).Tree(src, "result", ResultFile, false, func(p string, err error) { noLinkSkips = append(noLinkSkips, filepath.Base(p)) }); err != nil {
		t.Fatal(err)
	}
	if len(noLinkSkips) != 1 || noLinkSkips[0] != "latest" || bytes.Contains(buf2.Bytes(), []byte("\x00link\x00")) {
		t.Fatalf("links=false: skipped %v, stream has a link frame: %v", noLinkSkips, bytes.Contains(buf2.Bytes(), []byte("\x00link\x00")))
	}

	dst := t.TempDir()
	root, err := os.OpenRoot(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, f := range h.frames {
		rel := strings.TrimPrefix(f.name, "result/")
		switch f.kind {
		case FrameFile:
			if err := WriteFrameFile(root, rel, bytes.NewReader(f.body), f.n, f.mtime); err != nil {
				t.Fatal(err)
			}
		case FrameLink:
			if err := WriteFrameLink(root, rel, bytes.NewReader(f.body), f.n); err != nil {
				t.Fatal(err)
			}
		}
	}
	if fi, err := os.Stat(filepath.Join(dst, "a.txt")); err != nil || !fi.ModTime().Equal(old) {
		t.Fatalf("a.txt: %v %v", fi, err)
	}
	if link, err := os.Readlink(filepath.Join(dst, "latest")); err != nil || link != "a.txt" {
		t.Fatalf("latest -> %q %v", link, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dst, "result.json")); string(raw) != `{"status":"ok"}` {
		t.Fatalf("result.json = %q", raw)
	}
	entries, _ := os.ReadDir(dst)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
	// A body cut short leaves no file at the target.
	if err := WriteFrameFile(root, "short", strings.NewReader("ab"), 5, time.Time{}); err == nil {
		t.Fatal("short body accepted")
	}
	if _, err := os.Stat(filepath.Join(dst, "short")); err == nil {
		t.Fatal("a short body must leave no file")
	}
	// An event line appends with its newline.
	if err := AppendFrameLine(root, "hooks/live.jsonl", strings.NewReader("{}"), 2); err != nil {
		t.Fatal(err)
	}
	if err := AppendFrameLine(root, "hooks/live.jsonl", strings.NewReader("{}"), 2); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dst, "hooks", "live.jsonl")); string(raw) != "{}\n{}\n" {
		t.Fatalf("events = %q", raw)
	}
}

// Verifies ff8e85704b0a: the writers never leave the root — a link planted
// by an earlier frame cannot redirect a later file, an append, or a link
// replacement outside it, because every operation resolves inside the root.
func TestFrameWritersStayInRoot(t *testing.T) {
	base := t.TempDir()
	dst := filepath.Join(base, "result")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := WriteFrameLink(root, "evil", strings.NewReader("../outside"), 10); err != nil {
		t.Fatalf("a link with an outside target is inert text: %v", err)
	}
	if err := WriteFrameFile(root, "evil/pwned", strings.NewReader("owned"), 5, time.Time{}); err == nil {
		t.Fatal("a file through an escaping link was written")
	}
	if err := WriteFrameLink(root, "hooks/events.jsonl", strings.NewReader("../../rc"), 8); err != nil {
		t.Fatal(err)
	}
	if err := AppendFrameLine(root, "hooks/events.jsonl", strings.NewReader("evil"), 4); err == nil {
		t.Fatal("an append through an escaping link succeeded")
	}
	if err := WriteFrameLink(root, "evil/sub", strings.NewReader("x"), 1); err == nil {
		t.Fatal("a link under an escaping link was created")
	}
	if _, err := os.Stat(filepath.Join(base, "outside")); err == nil {
		t.Fatal("something was written outside the root")
	}
	if _, err := os.Stat(filepath.Join(base, "rc")); err == nil {
		t.Fatal("the append escaped the root")
	}
	// An escaping-but-dangling walk through ".." in the name is refused by
	// the reader; a cleaned name that stays inside is fine.
	if err := WriteFrameFile(root, "a/b/c.txt", strings.NewReader("c"), 1, time.Time{}); err != nil {
		t.Fatal(err)
	}
}

// Verifies ff8e85704b0a: the name rule.
func TestValidFrameName(t *testing.T) {
	for _, ok := range []string{"result/result.json", "sessions/a/b.jsonl", "handoff/bundle/CONTEXT.md", "x"} {
		if !ValidFrameName(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "/x", "a//b", "a/./b", "../a", "a/..", "a\\b", "a\x00b", "."} {
		if ValidFrameName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
