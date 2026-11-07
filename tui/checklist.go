package tui

import (
	"fmt"
	"strings"

	"github.com/dmitriyb/faber/config"
)

// PreflightClass is one line of the preflight checklist: a class of things
// the run would need, its state in the four-word vocabulary (ok, MISSING,
// FAIL, not checked), one precise fact, and the problems beneath it.
type PreflightClass struct {
	Name   string
	State  string
	Detail string
	Items  []PreflightItem
}

// PreflightItem is one problem of a class: who, what is wrong, and the fix
// when there is one (a command, a tag hash to look up).
type PreflightItem struct {
	Who, What, Fix string
}

// classNames is the checklist's fixed order.
var classNames = []string{"config", "images", "keys", "hooks", "credentials"}

// preflightClasses is Home's checklist: the whole project.
func preflightClasses(p *Project) []PreflightClass {
	return classesOf(p.Preflight, p.Templates, p.Preflight.Templates, true)
}

// needsClasses is the preview's checklist: the preflight narrowed to one
// workflow's templates, the config line left out (it has no per-workflow
// meaning).
func needsClasses(p *Project, ir *config.IR) []PreflightClass {
	n := needsOf(p, ir)
	var templates []TemplateInfo
	want := map[string]bool{}
	for _, c := range n.Images {
		want[c.Name] = true
	}
	for _, t := range p.Templates {
		if want[t.Name] {
			templates = append(templates, t)
		}
	}
	return classesOf(n, templates, n.Templates, false)
}

// classesOf folds the preflight's checks into the five classes.
func classesOf(pf Preflight, templates []TemplateInfo, templateCount int, withConfig bool) []PreflightClass {
	var out []PreflightClass
	if withConfig {
		out = append(out, PreflightClass{Name: "config", State: "ok", Detail: "valid"})
	}
	out = append(out, imagesClass(templates), keysClass(pf.Roles), hooksClass(pf.Slots, templateCount), credentialsClass(pf))
	return out
}

func imagesClass(templates []TemplateInfo) PreflightClass {
	c := PreflightClass{Name: "images"}
	failed, missing, hookBlocked, daemon, unchecked := 0, 0, 0, 0, 0
	for _, t := range templates {
		switch {
		case t.Err != "":
			failed++
			c.Items = append(c.Items, PreflightItem{Who: t.Name, What: t.Err})
		case t.Unchecked != "":
			hookBlocked++
			c.Items = append(c.Items, PreflightItem{Who: t.Name, What: hookWords(t.Unchecked)})
		case t.Probe != "":
			daemon++
			c.Items = append(c.Items, PreflightItem{Who: t.Name, What: t.Probe})
		case t.Checked && !t.Built:
			missing++
			c.Items = append(c.Items, PreflightItem{Who: t.Name, What: "faber build", Fix: tagHash(t.Tag)})
		case !t.Checked:
			unchecked++
		}
	}
	n := len(templates)
	switch {
	case failed > 0:
		c.State, c.Detail = "FAIL", fmt.Sprintf("%d of %d", failed, n)
	case missing > 0:
		c.State, c.Detail = "MISSING", fmt.Sprintf("%d of %d   faber build", missing, n)
	case hookBlocked > 0:
		c.State, c.Detail = "not checked", fmt.Sprintf("%d of %d, a hook is missing or broken", hookBlocked, n)
	case daemon > 0:
		c.State, c.Detail = "not checked", fmt.Sprintf("%d of %d, docker unreachable", daemon, n)
	case unchecked > 0:
		c.State, c.Detail = "not checked", fmt.Sprintf("%d of %d", unchecked, n)
	default:
		c.State, c.Detail = "ok", fmt.Sprintf("%d built", n)
	}
	return c
}

// hookWords turns the probe's "hooks.prelude not found" into the words the
// checklist speaks: "hook prelude missing".
func hookWords(unchecked string) string {
	slot, reason, _ := strings.Cut(strings.TrimPrefix(unchecked, "hooks."), " ")
	if reason == "not found" || reason == "" {
		reason = "missing"
	}
	return "hook " + slot + " " + reason
}

func keysClass(roles []Check) PreflightClass {
	c := PreflightClass{Name: "keys"}
	missing, unchecked, unused := 0, 0, 0
	for _, r := range roles {
		switch r.State {
		case "MISSING":
			missing++
			item := PreflightItem{Who: r.Name, What: r.Fix}
			if strings.HasPrefix(r.Fix, "faber add-key") {
				item.What, item.Fix = "not in the registry", r.Fix
			}
			c.Items = append(c.Items, item)
		case "not checked":
			unchecked++
			c.Items = append(c.Items, PreflightItem{Who: r.Name, What: "registry probe not wired"})
		case "unused":
			unused++
		}
	}
	n := len(roles)
	tail := ""
	if unused > 0 {
		tail = fmt.Sprintf(", %d unused", unused)
	}
	switch {
	case missing > 0:
		c.State, c.Detail = "MISSING", fmt.Sprintf("%d of %d roles%s", missing+unchecked, n, tail)
	case unchecked > 0:
		c.State, c.Detail = "not checked", fmt.Sprintf("%d of %d roles%s", unchecked, n, tail)
	default:
		c.State, c.Detail = "ok", fmt.Sprintf("%s%s", plural(n, "role"), tail)
	}
	return c
}

func hooksClass(slots []Check, templates int) PreflightClass {
	c := PreflightClass{Name: "hooks"}
	affected := map[string]bool{}
	for _, s := range slots {
		affected[s.Name] = true
		what := s.Fix + " missing"
		if path, reason, ok := strings.Cut(s.Fix, "   "); ok {
			what = path + " " + reason
		}
		c.Items = append(c.Items, PreflightItem{Who: s.Name + "/" + s.Detail, What: what})
	}
	if len(slots) > 0 {
		c.State, c.Detail = "MISSING", fmt.Sprintf("%s in %s", plural(len(slots), "slot"), plural(len(affected), "template"))
	} else {
		c.State, c.Detail = "ok", plural(templates, "template")
	}
	return c
}

func credentialsClass(pf Preflight) PreflightClass {
	c := PreflightClass{Name: "credentials"}
	services := plural(len(pf.Services), "service")
	// only file-mode services call the resolver; proxy and helper handles
	// need none
	fileMode := 0
	for _, s := range pf.Services {
		if s.Detail == "file" {
			fileMode++
		}
	}
	switch {
	case pf.Resolver.Name == "" && len(pf.Services) == 0:
		c.State, c.Detail = "ok", "none"
	case pf.Resolver.Name == "" && fileMode == 0:
		c.State, c.Detail = "ok", services+", no resolver needed"
	case pf.Resolver.Name == "":
		c.State, c.Detail = "MISSING", fmt.Sprintf("no resolver, %s in file mode", plural(fileMode, "service"))
		c.Items = append(c.Items, PreflightItem{Who: "resolver", What: fmt.Sprintf("not configured; %s need it", plural(fileMode, "file-mode service"))})
	case pf.Resolver.State == "MISSING":
		c.State, c.Detail = "MISSING", "resolver missing, "+services
		c.Items = append(c.Items, PreflightItem{Who: "resolver", What: pf.Resolver.Detail + " missing"})
	default:
		c.State, c.Detail = "ok", "resolver "+pf.Resolver.Detail+", "+services
	}
	return c
}

// checklistLines draws the checklist: a dim header, a line per class with
// the state in a fixed column, and beneath a class that is not ok up to
// budget problem rows as columns (who, what and the fix) then "+k more".
// sel marks the class line under the cursor (-1 for none).
func checklistLines(st style, classes []PreflightClass, w, budget, sel int) []string {
	cw := []int{15, 13, w - 28}
	// problem rows: indented under the class on a wide pane; on a narrow one
	// the indent and the name column shrink so the reason stays on screen
	indent, who := 18, 20
	if w < 50 {
		indent, who = 4, max(12, w/3)
	}
	iw := []int{indent + who, w - indent - who}
	out := []string{st.dim(cols([]string{"  PREFLIGHT", "status", "detail"}, cw))}
	for i, c := range classes {
		line := cols([]string{"  " + st.dim(c.Name), st.status(c.State), c.Detail}, cw)
		if i == sel {
			line = st.rev(pad(stripSGR(line), w))
		}
		out = append(out, line)
		if c.State == "ok" {
			continue
		}
		for j, it := range c.Items {
			if j == budget {
				out = append(out, strings.Repeat(" ", indent)+st.dim(fmt.Sprintf("+%d more", len(c.Items)-budget)))
				break
			}
			what := it.What
			// the fix joins the row only when it fits after the whole reason;
			// the drill-down always has it
			if it.Fix != "" && len(what)+3+len(it.Fix) < iw[1] {
				what += "   " + st.dim(it.Fix)
			}
			out = append(out, cols([]string{strings.Repeat(" ", indent) + st.ident(trunc(it.Who, who-1)), what}, iw))
		}
	}
	return out
}

// drillPane is one class opened from the checklist: the crumb, then every
// problem with its fix, the cursor's row in reverse, the window scrolled
// to keep it visible, as the runs table does.
func drillPane(st style, m *model, c PreflightClass, w, h int) []string {
	out := []string{st.chrome("PROJECT"), st.rule(w), "", "  " + st.chrome("PREFLIGHT") + st.dim(" > ") + st.ident(c.Name) + "   " + st.status(c.State) + "   " + c.Detail, ""}
	// three columns on a wide pane; on a narrow one the fix joins the what
	// column so the reason stays on screen
	who := clamp(w*3/10, 12, 22)
	three := w >= 70
	if three {
		out = append(out, st.dim(cols([]string{"  who", "what", "fix"}, []int{who + 2, 30, w - who - 32})))
	} else {
		out = append(out, st.dim(cols([]string{"  who", "what   fix"}, []int{who + 2, w - who - 2})))
	}
	avail := max(1, h-len(out))
	m.pfSel = clamp(m.pfSel, 0, len(c.Items)-1)
	start := 0
	if m.pfSel >= avail {
		start = m.pfSel - avail + 1
	}
	for i := start; i < len(c.Items) && i-start < avail; i++ {
		it := c.Items[i]
		var line string
		if three {
			line = cols([]string{"  " + st.ident(it.Who), it.What, it.Fix}, []int{who + 2, 30, w - who - 32})
		} else {
			what := it.What
			if it.Fix != "" {
				what += "   " + it.Fix
			}
			line = cols([]string{"  " + st.ident(it.Who), what}, []int{who + 2, w - who - 2})
		}
		if i == m.pfSel {
			line = st.rev(pad(stripSGR(line), w))
		}
		out = append(out, line)
	}
	if len(c.Items) == 0 {
		out = append(out, "  "+st.dim("nothing to fix"))
	}
	return out
}

// classByName finds a class of the checklist.
func classByName(classes []PreflightClass, name string) (PreflightClass, bool) {
	for _, c := range classes {
		if c.Name == name {
			return c, true
		}
	}
	return PreflightClass{}, false
}
