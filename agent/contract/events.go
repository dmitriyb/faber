package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

// Event is one line of the hooks events file: a hook's own account of
// something it did, for the operator. The engine never acts on one.
type Event struct {
	Line   int               // 1-based line in the file
	TS     time.Time         // when the hook wrote it
	Hook   string            // the writer's own name
	Name   string            // the event word
	Fields map[string]string // everything else, opaque
}

// Malformed is a line the reader could not accept, kept with its reason so
// a broken hook shows as broken instead of silent.
type Malformed struct {
	Line int
	Err  string
}

// Events is what ReadEvents returns: the accepted lines in file order, the
// rejected ones with their line numbers, and whether the file was longer
// than the read bound (the oldest lines are the ones not read).
type Events struct {
	Events    []Event
	Malformed []Malformed
	Truncated bool
}

// MaxEventsBytes bounds how much of the events file a reader takes in: the
// newest bytes, cut at a line boundary. A hook that logs without end must
// not make every view of its attempt read without end.
const MaxEventsBytes = 1 << 20

// MaxEventLineBytes bounds one event line; a longer line is malformed, not
// parsed in part.
const MaxEventLineBytes = 16 << 10

// SortedFieldKeys lists an event's extra fields in a stable order.
func (e Event) SortedFieldKeys() []string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ReadEvents reads the hooks events file at path. A missing file is no
// events and no error; any other read failure is returned. Lines are never
// dropped silently: a line that is not a JSON object, lacks ts, hook or
// event, carries a non-string field, or exceeds the line bound comes back
// as Malformed with its line number.
func ReadEvents(path string) (Events, error) {
	var out Events
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return out, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return out, err
	}
	// line numbering stays true to the file even when only its tail is read
	line := 0
	if st.Size() > MaxEventsBytes {
		out.Truncated = true
		if _, err := f.Seek(st.Size()-MaxEventsBytes, io.SeekStart); err != nil {
			return out, err
		}
		line = countLines(f, st.Size()-MaxEventsBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxEventsBytes+1))
	if err != nil {
		return out, err
	}
	if out.Truncated {
		// the first partial line belongs to an event the bound cut
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			raw = raw[i+1:]
			line++
		} else {
			raw = nil
		}
	}
	for _, l := range bytes.Split(raw, []byte("\n")) {
		line++
		l = bytes.TrimSpace(l)
		if len(l) == 0 {
			continue
		}
		ev, perr := parseEvent(l)
		if perr != nil {
			out.Malformed = append(out.Malformed, Malformed{Line: line, Err: perr.Error()})
			continue
		}
		ev.Line = line
		out.Events = append(out.Events, ev)
	}
	return out, nil
}

// countLines counts the newlines in the first n bytes of f, leaving the
// offset where it was.
func countLines(f *os.File, n int64) int {
	pos, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0
	}
	defer f.Seek(pos, io.SeekStart)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0
	}
	count := 0
	buf := make([]byte, 64<<10)
	for n > 0 {
		k, err := f.Read(buf[:min(int64(len(buf)), n)])
		count += bytes.Count(buf[:k], []byte("\n"))
		n -= int64(k)
		if err != nil {
			break
		}
	}
	return count
}

func parseEvent(l []byte) (Event, error) {
	if len(l) > MaxEventLineBytes {
		return Event{}, fmt.Errorf("line exceeds the %d-byte bound", MaxEventLineBytes)
	}
	var m map[string]any
	if err := json.Unmarshal(l, &m); err != nil || m == nil {
		return Event{}, errors.New("not a JSON object")
	}
	ev := Event{Fields: map[string]string{}}
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			return Event{}, fmt.Errorf("field %q is not a string", k)
		}
		switch k {
		case "ts":
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return Event{}, fmt.Errorf("ts %q is not RFC 3339", s)
			}
			ev.TS = t
		case "hook":
			ev.Hook = s
		case "event":
			ev.Name = s
		default:
			ev.Fields[k] = s
		}
	}
	switch {
	case ev.TS.IsZero():
		return Event{}, errors.New("no ts")
	case ev.Hook == "":
		return Event{}, errors.New("no hook")
	case ev.Name == "":
		return Event{}, errors.New("no event")
	}
	return ev, nil
}
