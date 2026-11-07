package box

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dmitriyb/faber/agent/contract"
)

// eventsPollInterval is how often the events tail looks for new lines.
const eventsPollInterval = 250 * time.Millisecond

// tailEvents streams the hooks' events file while the phases run: every
// complete new line of $FABER_RESULT_DIR/hooks/events.jsonl goes out as an
// event frame, so the operator's view of a hook's account is live. The
// returned stop flushes what is complete and ends the tail; a line without
// its newline waits for the shipping step, which sends the whole file.
func (b *Box) tailEvents(ctx context.Context) (stop func()) {
	if b.Frames == nil || b.Env.ResultDir == "" {
		return func() {}
	}
	path := filepath.Join(b.Env.ResultDir, filepath.FromSlash(contract.HooksEventsFile))
	var off int64
	var pending []byte
	flush := func() {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		if st, err := f.Stat(); err == nil && st.Size() < off {
			// The hook rewrote the file shorter: start over from its
			// beginning rather than waiting past its end forever.
			off, pending = 0, nil
		}
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return
		}
		data, err := io.ReadAll(f)
		if len(data) == 0 {
			return
		}
		off += int64(len(data))
		pending = append(pending, data...)
		for {
			i := bytes.IndexByte(pending, '\n')
			if i < 0 {
				break
			}
			line := pending[:i]
			pending = pending[i+1:]
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			if werr := b.Frames.Event(line); werr != nil {
				b.Log.WarnContext(ctx, "events tail", "err", werr)
			}
		}
		_ = err
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(eventsPollInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				flush()
				return
			case <-t.C:
				flush()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-finished
		})
	}
}

// Ship sends the attempt's files to the host over the frame channel, on
// every exit path of Main: everything under the result directory as
// result/<rel>, result.json last; with capture on, everything under the
// sessions directory as sessions/<rel>; then the end frame. A file that
// cannot be sent is logged and skipped, never cut. Nothing in the container
// writes to stdout but this and the events tail, so what the host reads is
// frames and nothing else.
func (b *Box) Ship(ctx context.Context) {
	if b.Frames == nil {
		return
	}
	skip := func(path string, err error) {
		b.Log.WarnContext(ctx, "ship: entry skipped", "path", path, "err", err)
	}
	if b.Env.ResultDir != "" {
		// No links among the records: the host refuses them there, so a
		// record could never be read through one.
		if err := b.Frames.Tree(b.Env.ResultDir, "result", contract.ResultFile, false, skip); err != nil {
			b.Log.ErrorContext(ctx, "ship result dir", "err", err)
		}
	}
	if b.Env.SessionsDir != "" {
		if err := b.Frames.Tree(b.Env.SessionsDir, "sessions", "", true, skip); err != nil {
			b.Log.ErrorContext(ctx, "ship sessions", "err", err)
		}
	}
	if err := b.Frames.End(); err != nil {
		b.Log.ErrorContext(ctx, "ship end", "err", err)
	}
}
