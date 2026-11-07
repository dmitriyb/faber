package contract

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The frame channel: the box's standard output is not a log but the stream
// its records travel on, so that no host directory has to be shared with the
// container. One record shape, both halves link this file:
//
//	\x00faber\x00<kind>\x00<name>\x00<mtime>\x00<len>\n<len bytes>
//
// The leading NUL keeps a text line from ever parsing as a header; a reader
// hands bytes outside any frame to a foreign sink instead of failing, so a
// toolset binary that writes to stdout costs an attempt nothing but noise.
const (
	FrameMagic = "\x00faber\x00"

	FrameEvent = "event" // one hooks/events.jsonl line, as the hook wrote it
	FrameFile  = "file"  // a regular file; the body is its content
	FrameLink  = "link"  // a symlink; the body is its target
	FrameEnd   = "end"   // the stream is complete; nothing follows

	// MaxFrameHeaderBytes bounds one header line; a name cannot be longer
	// than what fits in it.
	MaxFrameHeaderBytes = 4 << 10
)

// Frame-stream errors a reader returns. ErrStreamUnterminated is a stream
// that ended without an end frame: what was received stands, but the sender
// did not finish.
var (
	ErrFrameStream        = errors.New("contract: frame stream")
	ErrStreamUnterminated = errors.New("contract: frame stream ended without an end frame")
	// ErrShortBody is a body that ended before its declared length: the
	// writer padded the frame, so the stream stays readable, and the sender
	// decides whether the entry counts as skipped.
	ErrShortBody = errors.New("contract: frame body ended early; the rest was padded")
)

// ValidFrameName reports whether name is a relative slash path with no
// empty, "." or ".." segment, no leading slash, no backslash and no NUL —
// the containment rule a frame name must obey before it is joined under any
// root.
func ValidFrameName(name string) bool {
	if name == "" || strings.ContainsAny(name, "\x00\\") || strings.HasPrefix(name, "/") {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func validFrameKind(kind string) bool {
	switch kind {
	case FrameEvent, FrameFile, FrameLink, FrameEnd:
		return true
	}
	return false
}

// FrameWriter writes frames to one stream. It is safe for concurrent use:
// the events tail and the shipping step share the box's stdout.
type FrameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewFrameWriter wraps the stream.
func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{w: w} }

// Frame writes one frame: the header, then exactly n bytes of body. A body
// shorter than n is padded with NUL bytes so the stream stays aligned, and
// the short read is returned as an error: the receiver gets a file of the
// declared length, never a stream it cannot parse.
func (w *FrameWriter) Frame(kind, name string, mtime time.Time, body io.Reader, n int64) error {
	if !validFrameKind(kind) {
		return fmt.Errorf("%w: kind %q", ErrFrameStream, kind)
	}
	if kind == FrameEnd {
		if name != "" || n != 0 {
			return fmt.Errorf("%w: an end frame carries no name and no body", ErrFrameStream)
		}
	} else if !ValidFrameName(name) {
		return fmt.Errorf("%w: name %q is not a contained relative path", ErrFrameStream, name)
	}
	if n < 0 || n > MaxRecordBytes {
		return fmt.Errorf("%w: %s %s: body of %d bytes exceeds the %d-byte bound", ErrFrameStream, kind, name, n, MaxRecordBytes)
	}
	var stamp int64
	if !mtime.IsZero() {
		stamp = mtime.UnixNano()
	}
	header := FrameMagic + kind + "\x00" + name + "\x00" + strconv.FormatInt(stamp, 10) + "\x00" + strconv.FormatInt(n, 10) + "\n"
	if len(header) > MaxFrameHeaderBytes {
		return fmt.Errorf("%w: %s %s: header exceeds %d bytes", ErrFrameStream, kind, name, MaxFrameHeaderBytes)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := io.WriteString(w.w, header); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	copied, err := io.CopyN(w.w, body, n)
	if copied < n {
		if _, perr := io.CopyN(w.w, zeros{}, n-copied); perr != nil {
			return perr
		}
		if err == nil || errors.Is(err, io.EOF) {
			err = fmt.Errorf("%w: %s %s: %d of %d bytes", ErrShortBody, kind, name, copied, n)
		}
		return err
	}
	return err
}

// zeros is an endless reader of NUL bytes, the padding of a short body.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// Event writes one events-file line as an event frame. The trailing newline
// is the receiver's to add.
func (w *FrameWriter) Event(line []byte) error {
	line = bytes.TrimRight(line, "\r\n")
	return w.Frame(FrameEvent, HooksEventsFile, time.Time{}, bytes.NewReader(line), int64(len(line)))
}

// End writes the end frame.
func (w *FrameWriter) End() error {
	return w.Frame(FrameEnd, "", time.Time{}, nil, 0)
}

// Tree sends every entry under root: regular files as file frames named
// <prefix>/<rel> with their mtimes, symlinks as link frames when links is
// set (a harness's session tree needs its latest-pointer; nothing legitimate
// puts a link among the records, and the host refuses one there), other
// kinds skipped. last, a slash path relative to root, is sent after
// everything else (the box sends result.json last, so a broken stream never
// has a record ahead of the files it points at); "" sends nothing specially.
// An entry that cannot be sent — unreadable, over the frame bound, a link
// where none may go — is reported through skip and the walk continues; a
// missing root sends nothing. The stream error of a write ends the walk.
func (w *FrameWriter) Tree(root, prefix, last string, links bool, skip func(path string, err error)) error {
	if skip == nil {
		skip = func(string, error) {}
	}
	if _, err := os.Lstat(root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var deferred string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			skip(p, err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			skip(p, rerr)
			return nil
		}
		rel = filepath.ToSlash(rel)
		if last != "" && rel == last {
			deferred = p
			return nil
		}
		return w.entry(p, prefix+"/"+rel, d, links, skip)
	})
	if err != nil {
		return err
	}
	if deferred != "" {
		d, lerr := os.Lstat(deferred)
		if lerr != nil {
			skip(deferred, lerr)
			return nil
		}
		return w.entry(deferred, prefix+"/"+last, fs.FileInfoToDirEntry(d), links, skip)
	}
	return nil
}

// entry sends one non-directory entry. A stream error is returned; an entry
// problem is skipped.
func (w *FrameWriter) entry(p, name string, d fs.DirEntry, links bool, skip func(string, error)) error {
	switch {
	case d.Type()&fs.ModeSymlink != 0:
		if !links {
			skip(p, errors.New("a symlink where only files may go"))
			return nil
		}
		target, err := os.Readlink(p)
		if err != nil {
			skip(p, err)
			return nil
		}
		return w.Frame(FrameLink, name, time.Time{}, strings.NewReader(target), int64(len(target)))
	case d.Type().IsRegular():
		info, err := d.Info()
		if err != nil {
			skip(p, err)
			return nil
		}
		if info.Size() > MaxRecordBytes {
			skip(p, fmt.Errorf("%d bytes exceed the %d-byte frame bound", info.Size(), MaxRecordBytes))
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			skip(p, err)
			return nil
		}
		defer f.Close()
		err = w.Frame(FrameFile, name, info.ModTime(), f, info.Size())
		if errors.Is(err, ErrShortBody) {
			// The file shrank between stat and read: the frame went out
			// padded, the stream is intact, the entry is reported and the
			// walk goes on to the rest.
			skip(p, err)
			return nil
		}
		return err
	default:
		return nil
	}
}

// FrameHandler receives the frames of a stream. Frame gets the body as a
// reader bounded to n bytes; whatever it leaves unread is drained. Foreign
// gets the bytes that arrive outside any frame. A non-nil return ends the
// read.
type FrameHandler interface {
	Frame(kind, name string, mtime time.Time, body io.Reader, n int64) error
	Foreign(p []byte) error
}

// ReadFrames reads frames from r until the end frame, handing each to h. It
// never buffers a body. It returns nil after the end frame,
// ErrStreamUnterminated when r ends before one, a wrapped ErrFrameStream
// for a header that violates the format or a body cut short, and the
// handler's error verbatim.
func ReadFrames(r io.Reader, h FrameHandler) error {
	br := bufio.NewReaderSize(r, 2*MaxFrameHeaderBytes)
	for {
		head, err := br.Peek(len(FrameMagic))
		if len(head) == 0 && err != nil {
			if errors.Is(err, io.EOF) {
				return ErrStreamUnterminated
			}
			return err
		}
		if string(head) == FrameMagic {
			done, err := readOne(br, h)
			if err != nil || done {
				return err
			}
			continue
		}
		// Not a header here: foreign bytes up to the next NUL, which may
		// start one. A NUL that does not start a header is foreign too.
		if head[0] == 0 {
			br.Discard(1)
			if err := h.Foreign([]byte{0}); err != nil {
				return err
			}
			continue
		}
		chunk, rerr := br.ReadSlice(0)
		if rerr == nil {
			chunk = chunk[:len(chunk)-1]
			if err := br.UnreadByte(); err != nil {
				return err
			}
		}
		if len(chunk) > 0 {
			if err := h.Foreign(append([]byte(nil), chunk...)); err != nil {
				return err
			}
		}
		switch {
		case rerr == nil, errors.Is(rerr, bufio.ErrBufferFull):
		case errors.Is(rerr, io.EOF):
			return ErrStreamUnterminated
		default:
			return rerr
		}
	}
}

// readOne parses one header at the reader's position and hands the frame
// to h. done reports the end frame.
func readOne(br *bufio.Reader, h FrameHandler) (done bool, err error) {
	line, err := br.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) || errors.Is(err, io.EOF) {
			return false, fmt.Errorf("%w: header longer than %d bytes or cut short", ErrFrameStream, MaxFrameHeaderBytes)
		}
		return false, err
	}
	if len(line) > MaxFrameHeaderBytes {
		return false, fmt.Errorf("%w: header longer than %d bytes", ErrFrameStream, MaxFrameHeaderBytes)
	}
	fields := strings.Split(string(line[len(FrameMagic):len(line)-1]), "\x00")
	if len(fields) != 4 {
		return false, fmt.Errorf("%w: header has %d fields, want kind, name, mtime, len", ErrFrameStream, len(fields))
	}
	kind, name := fields[0], fields[1]
	if !validFrameKind(kind) {
		return false, fmt.Errorf("%w: kind %q", ErrFrameStream, kind)
	}
	stamp, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return false, fmt.Errorf("%w: %s %s: mtime %q", ErrFrameStream, kind, name, fields[2])
	}
	n, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil || n < 0 || n > MaxRecordBytes {
		return false, fmt.Errorf("%w: %s %s: length %q outside [0, %d]", ErrFrameStream, kind, name, fields[3], MaxRecordBytes)
	}
	if kind == FrameEnd {
		if name != "" || n != 0 {
			return false, fmt.Errorf("%w: an end frame carries no name and no body", ErrFrameStream)
		}
	} else if !ValidFrameName(name) {
		return false, fmt.Errorf("%w: name %q is not a contained relative path", ErrFrameStream, name)
	}
	var mtime time.Time
	if stamp != 0 {
		mtime = time.Unix(0, stamp)
	}
	body := &countingReader{r: io.LimitReader(br, n)}
	herr := h.Frame(kind, name, mtime, body, n)
	if _, derr := io.Copy(io.Discard, body); derr != nil {
		return false, derr
	}
	if body.n < n {
		return false, fmt.Errorf("%w: %s %s: body ended after %d of %d bytes", ErrFrameStream, kind, name, body.n, n)
	}
	if herr != nil {
		return false, herr
	}
	return kind == FrameEnd, nil
}

// countingReader counts the bytes read through it, so a short body is told
// apart from a handler that left bytes unread.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	c.n += int64(k)
	return k, err
}

// The receiving side writes only through an os.Root: a name is validated
// lexically by the reader, but the sender may have planted a symlink under
// an earlier frame, and a path resolved through it would leave the tree. The
// root refuses any component that resolves outside it, in the kernel.

// WriteFrameFile writes a file frame's body at rel (a slash path) under
// root through a temporary name beside it, applies the frame's mtime when
// it has one, and renames it into place: a stream that breaks mid-body
// leaves no partial file. Parent directories are created.
func WriteFrameFile(root *os.Root, rel string, body io.Reader, n int64, mtime time.Time) error {
	rel = filepath.FromSlash(rel)
	dir := filepath.Dir(rel)
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmpName := filepath.Join(dir, "."+filepath.Base(rel)+"."+randomSuffix())
	tmp, err := root.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		tmp.Close()
		root.Remove(tmpName)
		return err
	}
	if copied, err := io.CopyN(tmp, body, n); err != nil || copied != n {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if !mtime.IsZero() {
		if err := root.Chtimes(tmpName, mtime, mtime); err != nil {
			return fail(err)
		}
	}
	if err := root.Rename(tmpName, rel); err != nil {
		return fail(err)
	}
	return nil
}

// WriteFrameLink creates rel under root as a symlink to the link frame's
// body, replacing a file or link at that name. The target is text: a link
// that points outside the root is inert for every later root operation,
// which refuses to resolve through it.
func WriteFrameLink(root *os.Root, rel string, body io.Reader, n int64) error {
	if n > MaxFrameHeaderBytes {
		return fmt.Errorf("%w: link target of %d bytes", ErrFrameStream, n)
	}
	raw, err := io.ReadAll(io.LimitReader(body, n))
	if err != nil {
		return err
	}
	if int64(len(raw)) != n {
		return io.ErrUnexpectedEOF
	}
	rel = filepath.FromSlash(rel)
	if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return root.Symlink(string(raw), rel)
}

// AppendFrameLine appends an event frame's body plus a newline to rel under
// root, creating the file and its parents.
func AppendFrameLine(root *os.Root, rel string, body io.Reader, n int64) error {
	rel = filepath.FromSlash(rel)
	if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.CopyN(f, body, n); err != nil {
		return err
	}
	_, err = f.Write([]byte{'\n'})
	return err
}

// randomSuffix is the temporary name's tail.
func randomSuffix() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
