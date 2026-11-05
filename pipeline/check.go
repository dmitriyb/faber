package pipeline

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dmitriyb/faber/agent/contract"
	"github.com/dmitriyb/faber/config"
)

// CheckLine is a check run's account of one executed step: which hook slots
// its template declares, how many events its hooks wrote, and — when it
// failed — the phase and reason, so the operator reads which hook needs
// what the sandbox withheld.
type CheckLine struct {
	ID     string   `json:"id"`
	Status string   `json:"status"`
	Hooks  []string `json:"hooks,omitempty"`
	Events int      `json:"events"`
	Phase  string   `json:"phase,omitempty"`
	Reason string   `json:"reason,omitempty"`
	Detail string   `json:"detail,omitempty"`
}

// Text is the line as the human report prints it.
func (c CheckLine) Text() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-12s %s", statusWord(c.Status), c.ID)
	if len(c.Hooks) > 0 {
		fmt.Fprintf(&sb, "  hooks %s", strings.Join(c.Hooks, " "))
	}
	fmt.Fprintf(&sb, "  events %d", c.Events)
	if c.Phase != "" {
		fmt.Fprintf(&sb, "  phase %s", c.Phase)
	}
	if c.Reason != "" {
		fmt.Fprintf(&sb, "  %s", c.Reason)
		if c.Detail != "" {
			fmt.Fprintf(&sb, ": %s", sanitizeTerm(c.Detail))
		}
	}
	return sb.String()
}

// checkLines derives the check account from what the run left on disk:
// the report's executed steps, the templates the IRs resolve them to, each
// attempt's events file, and a failed attempt's handoff record for the
// phase.
func checkLines(r *RunReport, runDir string, ir *config.IR, workflows map[string]*config.IR) []CheckLine {
	templates := map[string]*config.ResolvedTemplate{}
	var walk func(*config.IR)
	walk = func(g *config.IR) {
		if g == nil {
			return
		}
		for i := range g.Nodes {
			n := &g.Nodes[i]
			if n.Template != nil {
				templates[n.ID] = n.Template
			}
			walk(n.Sub)
		}
	}
	walk(ir)
	for _, name := range sortedKeys(workflows) {
		walk(workflows[name])
	}
	var genIDs []string
	for _, g := range r.Generate {
		genIDs = append(genIDs, g.Node)
	}
	templateOf := func(id string) *config.ResolvedTemplate {
		if t := templates[id]; t != nil {
			return t
		}
		if gen, item, ok := instanceOf(id, genIDs); ok {
			return templates[strings.TrimPrefix(id, gen+"["+item+"]/")]
		}
		return nil
	}
	var lines []StepLine
	lines = append(lines, r.Steps...)
	for _, g := range r.Generate {
		for _, it := range g.Items {
			lines = append(lines, it.Steps...)
		}
	}
	var out []CheckLine
	for _, l := range lines {
		if l.Cached || (l.Status != StateOK && l.Status != StateFailed && l.Status != StateHalted) {
			continue
		}
		c := CheckLine{ID: l.ID, Status: l.Status}
		if t := templateOf(l.ID); t != nil {
			for _, h := range [][2]string{{"context", t.Hooks.Context}, {"prelude", t.Hooks.Prelude}, {"postlude", t.Hooks.Postlude}, {"on_failure", t.Hooks.OnFailure}} {
				if h[1] != "" {
					c.Hooks = append(c.Hooks, h[0])
				}
			}
		}
		attempt := l.Attempts
		if attempt < 1 {
			attempt = 1
		}
		if ev, err := AttemptEvents(runDir, l.ID, attempt); err == nil {
			c.Events = len(ev.Events)
		}
		if l.Error != nil {
			c.Reason, c.Detail = l.Error.Reason, l.Error.Detail
			if l.Error.Handoff != "" {
				if raw, err := contract.ReadBoundedFile(filepath.Join(runDir, l.Error.Handoff)); err == nil {
					var h contract.Handoff
					if json.Unmarshal(raw, &h) == nil {
						c.Phase = h.Phase
					}
				}
			}
		}
		if l.Halt != nil {
			c.Phase, c.Reason, c.Detail = l.Halt.Phase, l.Halt.Reason, l.Halt.Detail
		}
		out = append(out, c)
	}
	return out
}
