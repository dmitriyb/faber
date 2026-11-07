package pipeline

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dmitriyb/faber/agent/contract"
)

// MaxAttemptStreamBytes bounds what one attempt's frame stream may deposit
// under its attempt directory. The box is untrusted; a transcript or a
// result file is never this large, a hostile box must not fill the disk.
const MaxAttemptStreamBytes = 1 << 30

// demuxer is the host end of the box's frame channel: an io.Writer the
// container runner feeds the container's stdout into, demultiplexed by the
// contract reader into the attempt directory as it streams. Event frames
// append to the result dir's hooks/events.jsonl; file and link frames land
// under result/ or sessions/ — the two roots a name may start with — through
// temporary names, so a stream that breaks mid-frame leaves no partial file;
// bytes outside any frame go to the human log with one warning. The first
// violation (an escaping name, an unknown kind, a frame or an attempt over
// its bound) is kept, and every later frame is drained and discarded: the
// box sends result.json last, so the attempt then extracts as box-vanished,
// which the caller annotates with Err.
type demuxer struct {
	attemptDir string
	log        io.Writer // output.log, for foreign bytes
	logger     *slog.Logger
	limit      int64 // MaxAttemptStreamBytes, lowered by tests

	// roots are the two trees a frame may land in, opened on first use;
	// every write goes through them so a box-planted link cannot redirect a
	// later frame outside the attempt directory.
	roots map[string]*os.Root

	pw   *io.PipeWriter
	done chan struct{}

	mu       sync.Mutex
	total    int64
	err      error // the first violation
	ended    bool  // the end frame arrived
	foreign  bool  // warned once
	readErr  error // the contract reader's verdict on the stream
	warnOnce sync.Once
}

// newDemuxer starts reading; the returned writer is the container's stdout.
func newDemuxer(attemptDir string, log io.Writer, logger *slog.Logger) *demuxer {
	pr, pw := io.Pipe()
	d := &demuxer{attemptDir: attemptDir, log: log, logger: logger, limit: MaxAttemptStreamBytes, roots: map[string]*os.Root{}, pw: pw, done: make(chan struct{})}
	go func() {
		defer close(d.done)
		err := contract.ReadFrames(pr, d)
		// Whatever the verdict, keep the writer from blocking until the
		// container is gone.
		io.Copy(io.Discard, pr)
		d.mu.Lock()
		d.readErr = err
		d.mu.Unlock()
	}()
	return d
}

func (d *demuxer) Write(p []byte) (int, error) { return d.pw.Write(p) }

// Close ends the stream and waits for the reader. It returns the first
// violation, or the reader's own stream error, or nil for a clean stream.
func (d *demuxer) Close() error {
	d.pw.Close()
	<-d.done
	for _, r := range d.roots {
		r.Close()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return d.err
	}
	if d.readErr != nil && !errors.Is(d.readErr, contract.ErrStreamUnterminated) {
		return d.readErr
	}
	return nil
}

// Ended reports whether the end frame arrived.
func (d *demuxer) Ended() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ended
}

// violate records the first violation; later frames are discarded.
func (d *demuxer) violate(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err == nil {
		d.err = err
		d.logger.Warn("frame stream violation; later frames are discarded", "attempt", d.attemptDir, "err", err)
	}
}

func (d *demuxer) closed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err != nil
}

// Frame implements contract.FrameHandler. It never returns an error: the
// reader must keep draining what the container writes.
func (d *demuxer) Frame(kind, name string, mtime time.Time, body io.Reader, n int64) error {
	if d.closed() {
		return nil
	}
	d.mu.Lock()
	d.total += n
	over := d.total > d.limit
	d.mu.Unlock()
	if over {
		d.violate(fmt.Errorf("attempt stream exceeds the %d-byte bound", d.limit))
		return nil
	}
	switch kind {
	case contract.FrameEnd:
		d.mu.Lock()
		d.ended = true
		d.mu.Unlock()
	case contract.FrameEvent:
		if name != contract.HooksEventsFile {
			d.violate(fmt.Errorf("event frame named %q, want %s", name, contract.HooksEventsFile))
			return nil
		}
		if err := d.appendEvent(body, n); err != nil {
			d.violate(fmt.Errorf("append event: %w", err))
		}
	case contract.FrameFile, contract.FrameLink:
		root, rel, err := d.target(name)
		if err != nil {
			d.violate(err)
			return nil
		}
		// A link among the records would let the extractor read a record
		// through it; nothing legitimate puts one there. Sessions keep
		// theirs: a harness's latest-pointer is a real link.
		if kind == contract.FrameLink && strings.HasPrefix(name, "result/") {
			d.violate(fmt.Errorf("link %s: no links under result/", name))
			return nil
		}
		if kind == contract.FrameFile {
			err = contract.WriteFrameFile(root, rel, body, n, mtime)
		} else {
			err = contract.WriteFrameLink(root, rel, body, n)
		}
		// A body that ends early is the reader's finding (it names the
		// bytes); the write just leaves no file.
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			d.violate(fmt.Errorf("%s %s: %w", kind, name, err))
		}
	}
	return nil
}

// target maps a frame name onto one of the two roots, or refuses it. The
// contract reader already proved the name lexically contained; the root
// keeps it contained through whatever the box planted earlier.
func (d *demuxer) target(name string) (*os.Root, string, error) {
	rootName, rel, ok := strings.Cut(name, "/")
	if !ok || (rootName != "result" && rootName != "sessions") {
		return nil, "", fmt.Errorf("frame %q: root is neither result/ nor sessions/", name)
	}
	root, err := d.root(rootName)
	if err != nil {
		return nil, "", err
	}
	return root, rel, nil
}

// root opens <attempt>/<name> as an os.Root on first use.
func (d *demuxer) root(name string) (*os.Root, error) {
	if r, ok := d.roots[name]; ok {
		return r, nil
	}
	dir := filepath.Join(d.attemptDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	d.roots[name] = r
	return r, nil
}

// appendEvent adds one line to the attempt's events file, the file the live
// view and the on_failure hook read.
func (d *demuxer) appendEvent(body io.Reader, n int64) error {
	root, err := d.root("result")
	if err != nil {
		return err
	}
	return contract.AppendFrameLine(root, contract.HooksEventsFile, body, n)
}

// Foreign implements contract.FrameHandler: a writer on the container's
// stdout that is not the box costs the attempt nothing but noise.
func (d *demuxer) Foreign(p []byte) error {
	d.warnOnce.Do(func() {
		d.logger.Warn("bytes outside any frame on the box's stdout; appended to output.log", "attempt", d.attemptDir)
	})
	if d.log != nil {
		d.log.Write(p)
	}
	return nil
}
