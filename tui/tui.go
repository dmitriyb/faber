// Package tui is the live view: a reader over the run store, the reporter,
// and the IR that renders Home and Run screens on a terminal. It executes
// nothing in-process — runs it starts are detached `faber run` children it
// then watches through the journal like any other — and it shows nothing
// the CLI cannot show: every screen is a command rendered live, and every
// action names the command it runs.
package tui

import (
	"context"
	"time"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
)

// ImageProbe is the slice of the image builder and the docker client Home's
// images pane needs: the tag a template resolves to, and whether the daemon
// holds it. nil skips the pane.
type ImageProbe interface {
	Tag(template *config.ResolvedTemplate) (string, error)
	Exists(ctx context.Context, tag string) (bool, error)
}

// Options is the wiring's injection: every capability explicit, no globals.
type Options struct {
	Store      *failure.Store // run journals (required)
	Images     ImageProbe     // nil = no images pane
	LogDir     string         // where detached children write their stderr
	Executable string         // the faber binary spawned for run/resume; "" = os.Executable()
	Now        func() time.Time
}

// UI implements config.UIRunner.
type UI struct {
	opts Options
}

// New builds the view over the given capabilities.
func New(o Options) *UI {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &UI{opts: o}
}

// Run implements config.UIRunner: it owns the terminal until the operator
// quits, then prints the selected command on stdout (if any) so it sits on
// the next prompt line.
func (u *UI) Run(ctx context.Context, uo config.UIOptions) error {
	app := newApp(u.opts, uo)
	return app.loop(ctx)
}
