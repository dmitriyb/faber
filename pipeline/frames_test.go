package pipeline

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmitriyb/faber/agent/contract"
	"github.com/dmitriyb/faber/failure"
)

// attemptPath is the host path of one attempt file.
func attemptPath(box BoxAttempt, parts ...string) string {
	return filepath.Join(append([]string{box.RunDir, "boxes", pathToken(box.NodeID), "attempt-1"}, parts...)...)
}

// okRecord is a shipped ok record.
func okRecord(w *contract.FrameWriter) {
	rec := []byte(`{"status":"ok","contract":2,"payload":{"out":"d"},"attempt":1}`)
	w.Frame(contract.FrameFile, "result/"+contract.ResultFile, time.Time{}, bytes.NewReader(rec), int64(len(rec)))
}

func file(w *contract.FrameWriter, name, body string, mtime time.Time) {
	w.Frame(contract.FrameFile, name, mtime, strings.NewReader(body), int64(len(body)))
}

// Verifies pipeline test 22b: the demuxer lands the box's frames under the
// attempt directory — event lines appended as they arrive, files with their
// mtimes under result/ and sessions/, result.json read by the extractor —
// and foreign bytes on the box's stdout go to output.log.
func TestBoxRun_DemuxerLandsFrames(t *testing.T) {
	mtime := testBase.Add(-time.Hour)
	containers := &fakeContainers{
		output: []byte("box log line\n"),
		frames: func(w *contract.FrameWriter) {
			w.Event([]byte(`{"ts":"2026-11-07T10:00:00Z","hook":"h","event":"one"}`))
			w.Event([]byte(`{"ts":"2026-11-07T10:00:01Z","hook":"h","event":"two"}`))
			w.Frame(contract.FrameFile, "result/output.json", mtime, strings.NewReader(`{"out":"d"}`), 11)
			file(w, "result/hooks/events.jsonl", `{"ts":"2026-11-07T10:00:00Z","hook":"h","event":"one"}`+"\n"+`{"ts":"2026-11-07T10:00:01Z","hook":"h","event":"two"}`+"\n", time.Time{})
			file(w, "sessions/t.jsonl", "{}\n", mtime)
			okRecord(w)
			w.End()
		},
	}
	boxes := &AgentBoxes{Containers: containers, Bindings: &fakeBindings{}}
	box := boxAttempt(t)
	got, err := boxes.RunAttempt(context.Background(), box)
	if err != nil {
		t.Fatalf("run attempt: %v", err)
	}
	if got.Result.Status != failure.StatusOK || string(got.Result.Payload) != `{"out":"d"}` {
		t.Fatalf("result = %+v", got.Result)
	}
	events, err := AttemptEvents(box.RunDir, box.NodeID, 1)
	if err != nil || len(events.Events) != 2 || events.Events[1].Name != "two" || len(events.Malformed) != 0 {
		t.Fatalf("events = %+v (%v)", events, err)
	}
	if fi, err := os.Stat(attemptPath(box, "result", "output.json")); err != nil || !fi.ModTime().Equal(mtime) {
		t.Fatalf("output.json: %v %v", fi, err)
	}
	if fi, err := os.Stat(attemptPath(box, "sessions", "t.jsonl")); err != nil || !fi.ModTime().Equal(mtime) {
		t.Fatalf("sessions/t.jsonl: %v %v", fi, err)
	}
	log, _ := os.ReadFile(attemptPath(box, OutputLogName))
	if string(log) != "box log line\n" {
		t.Fatalf("output.log = %q", log)
	}
	entries, _ := os.ReadDir(attemptPath(box, "result"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

// Verifies pipeline test 22b: bytes outside any frame reach output.log, and
// a stream that ships the record without an end frame still stands.
func TestBoxRun_DemuxerForeignBytesAndNoEnd(t *testing.T) {
	containers := &fakeContainers{output: []byte("stderr line\n")}
	boxes := &AgentBoxes{Containers: containers, Bindings: &fakeBindings{}}
	box := boxAttempt(t)
	// Foreign bytes go straight to the spec's Stdout, before the frames
	// (the fake's own hook cannot write them, so it is replaced here).
	containers.frames = func(w *contract.FrameWriter) {
		raw := containers.specs[len(containers.specs)-1].Stdout
		raw.Write([]byte("a toolset binary printed this\n"))
		okRecord(w)
	}
	got, err := boxes.RunAttempt(context.Background(), box)
	if err != nil {
		t.Fatalf("run attempt: %v", err)
	}
	if got.Result.Status != failure.StatusOK {
		t.Fatalf("a record without an end frame must stand: %+v", got.Result)
	}
	log, _ := os.ReadFile(attemptPath(box, OutputLogName))
	if !strings.Contains(string(log), "a toolset binary printed this\n") || !strings.Contains(string(log), "stderr line\n") {
		t.Fatalf("output.log = %q, want the foreign bytes beside the stderr", log)
	}
}

// Verifies pipeline test 22b: a violation — an escaping or foreign-rooted
// name, a truncated frame, an attempt over its byte bound — closes the
// demuxer, nothing is written for the offending frame, later frames are
// discarded, and the attempt extracts as box-vanished naming the violation
// (result.json is shipped last, so it never arrives).
func TestBoxRun_DemuxerViolations(t *testing.T) {
	cases := map[string]struct {
		frames func(w *contract.FrameWriter, raw func(string))
		want   string
		absent []string
	}{
		"foreign root": {
			frames: func(w *contract.FrameWriter, raw func(string)) {
				file(w, "other/x", "x", time.Time{})
				okRecord(w)
				w.End()
			},
			want:   "neither result/ nor sessions/",
			absent: []string{"other/x", "result/result.json"},
		},
		"escaping name": {
			frames: func(w *contract.FrameWriter, raw func(string)) {
				raw(contract.FrameMagic + "file\x00result/../../x\x000\x001\nX")
				okRecord(w)
				w.End()
			},
			want:   "contained",
			absent: []string{"../../x", "result/result.json"},
		},
		"truncated frame": {
			frames: func(w *contract.FrameWriter, raw func(string)) {
				raw(contract.FrameMagic + "file\x00result/result.json\x000\x00500\n{\"status\":\"ok\"")
			},
			want:   "body ended",
			absent: []string{"result/result.json"},
		},
		"over the attempt bound": {
			frames: func(w *contract.FrameWriter, raw func(string)) {
				file(w, "result/big", strings.Repeat("x", 2048), time.Time{})
				okRecord(w)
				w.End()
			},
			want:   "exceeds the 1024-byte bound",
			absent: []string{"result/big", "result/result.json"},
		},
		// A link among the records is refused outright (a record must never
		// be read through one), and a link planted under sessions/ cannot
		// redirect a later file out of the attempt directory: every write
		// resolves inside an os.Root of result/ or sessions/.
		"link under result": {
			frames: func(w *contract.FrameWriter, raw func(string)) {
				w.Frame(contract.FrameLink, "result/evil", time.Time{}, strings.NewReader("../../outside"), 13)
				file(w, "result/evil/pwned", "owned", time.Time{})
				okRecord(w)
				w.End()
			},
			want:   "no links under result/",
			absent: []string{"result/evil", "../../outside/pwned", "result/result.json"},
		},
		"file through a planted session link": {
			frames: func(w *contract.FrameWriter, raw func(string)) {
				w.Frame(contract.FrameLink, "sessions/evil", time.Time{}, strings.NewReader("../../../outside"), 16)
				file(w, "sessions/evil/pwned", "owned", time.Time{})
				okRecord(w)
				w.End()
			},
			want:   "sessions/evil/pwned",
			absent: []string{"../../../outside/pwned", "result/result.json"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			containers := &fakeContainers{}
			containers.frames = func(w *contract.FrameWriter) {
				stdout := containers.specs[len(containers.specs)-1].Stdout
				tc.frames(w, func(s string) { stdout.Write([]byte(s)) })
			}
			boxes := &AgentBoxes{Containers: containers, Bindings: &fakeBindings{}}
			if name == "over the attempt bound" {
				boxes.streamLimit = 1024
			}
			box := boxAttempt(t)
			got, err := boxes.RunAttempt(context.Background(), box)
			if err != nil {
				t.Fatalf("run attempt: %v", err)
			}
			if got.Result.Status != failure.StatusFailed || got.Result.Error.Reason != contract.ReasonBoxVanished {
				t.Fatalf("result = %+v, want box-vanished", got.Result)
			}
			if !strings.Contains(got.Result.Error.Detail, "frame stream:") || !strings.Contains(got.Result.Error.Detail, tc.want) {
				t.Fatalf("detail %q lacks the violation %q", got.Result.Error.Detail, tc.want)
			}
			for _, rel := range tc.absent {
				if _, err := os.Stat(attemptPath(box, filepath.FromSlash(rel))); err == nil {
					t.Errorf("%s was written despite the violation", rel)
				}
			}
		})
	}
}
