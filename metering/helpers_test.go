package metering

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strconv"
	"testing"
)

// stubMeter is a scriptable Meter for ledger tests.
type stubMeter struct {
	units      []Unit
	estimateFn func(s Step) (Estimate, error)
	actualFn   func(r ResultView) ([]Cost, error)
}

func (m *stubMeter) Units() []Unit { return m.units }

func (m *stubMeter) Estimate(_ context.Context, s Step) (Estimate, error) {
	if m.estimateFn == nil {
		return Estimate{}, nil
	}
	return m.estimateFn(s)
}

func (m *stubMeter) Actual(_ context.Context, r ResultView) ([]Cost, error) {
	if m.actualFn == nil {
		return nil, nil
	}
	return m.actualFn(r)
}

// fixedMeter builds a stubMeter that always estimates the given costs and
// settles the given actuals.
func fixedMeter(units []Unit, est []Cost, actual []Cost) *stubMeter {
	return &stubMeter{
		units:      units,
		estimateFn: func(Step) (Estimate, error) { return Estimate{Costs: est}, nil },
		actualFn:   func(ResultView) ([]Cost, error) { return actual, nil },
	}
}

// fakeRunner is a scripted ProbeRunner: fn receives the zero-based call
// number, the argv, and the fully read stdin.
type fakeRunner struct {
	fn     func(call int, argv []string, stdin string) ([]byte, error)
	calls  int
	stdins []string
}

func (f *fakeRunner) Run(_ context.Context, argv []string, stdin io.Reader) ([]byte, error) {
	var in string
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, err
		}
		in = string(b)
	}
	call := f.calls
	f.calls++
	f.stdins = append(f.stdins, in)
	return f.fn(call, argv, in)
}

// testLogger returns a logger writing text records into the returned buffer.
func testLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// wantCosts asserts the exact cost slice.
func wantCosts(t *testing.T, got []Cost, want ...Cost) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d costs %v, want %d costs %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cost[%d]: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// rateLimitRecord builds the box-contract failure record; reset 0 sends an
// empty detail payload (signal without a reset epoch).
func rateLimitRecord(node string, reset int64) FailureRecord {
	rec := FailureRecord{NodeID: node, Status: StatusFailed, Reason: ReasonRateLimit}
	if reset != 0 {
		rec.Detail = []byte(`{"reset": ` + strconv.FormatInt(reset, 10) + `}`)
	}
	return rec
}
