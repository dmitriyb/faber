package pipeline

import "time"

// Clock is the scheduler's injectable time source: Now stamps records and
// defer annotations, AfterFunc arms defer wakes. Tests substitute a manual
// clock so defer/re-admission scenarios never sleep.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func())
}

// realClock is the production Clock over the time package.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, f func()) { time.AfterFunc(d, f) }
