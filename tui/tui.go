// Package tui is the live view: a reader over the run store, the reporter,
// and the IR that renders Home and Run screens on a terminal. It executes
// nothing in-process — runs it starts are detached `faber run` children it
// then watches through the journal like any other — and it shows nothing
// the CLI cannot show: every screen is a command rendered live, and every
// action names the command it runs.
package tui

import (
	"context"
	"os"
	"os/signal"
	"syscall"
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

// KeyProbe resolves a role's key the way a run would before it starts a
// box — the registry and the key locator, nothing spawned — so Home can
// say whether a used role has a key. nil leaves registry roles "not checked".
type KeyProbe interface {
	Resolve(ctx context.Context, role string, def config.IdentityDef) (source string, err error)
}

// Options is the wiring's injection: every capability explicit, no globals.
type Options struct {
	Store      *failure.Store // run journals (required)
	Images     ImageProbe     // nil = images "not checked"
	Keys       KeyProbe       // nil = registry roles "not checked"
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
	// A kill must restore the terminal: raw mode has no ISIG, so Ctrl-C
	// arrives as a byte (handled as quit), while SIGTERM and SIGHUP cancel
	// the context, which ends the loop and runs the deferred restore.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	app := newApp(u.opts, uo)
	return app.loop(ctx)
}
