package infra

import (
	"bytes"
	"debug/elf"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/dmitriyb/faber/config"
)

// ImageInputs are the engine's static inputs baked beside the toolset: the
// template's resolved hook paths (context, prelude and postlude are baked;
// on_failure runs on the host and is not) and its resolved skills leg (nil
// means no leg). The faber-box binary is the builder's own input.
type ImageInputs struct {
	Hooks  config.HookSet
	Skills *config.ResolvedSkills
}

// stagedHook is one declared in-box hook, read once.
type stagedHook struct {
	slot string
	data []byte
}

// stagedFile is one regular file of the skills tree, read once: its path
// relative to /faber/skills with forward slashes, whether it is executable,
// and its bytes.
type stagedFile struct {
	rel  string
	exec bool
	data []byte
}

// stagedInputs is everything a tag hashes and a build stages, read exactly
// once per operation so the two can never disagree.
type stagedInputs struct {
	overlay    []byte
	hasOverlay bool
	box        []byte       // nil for a proof, which bakes nothing
	hooks      []stagedHook // declared slots in fixed order
	skills     []stagedFile // sorted by rel
	hasSkills  bool
}

// inBoxSlots are the hook slots that run inside the box, in the order their
// bytes fold into the tag. on_failure is a host-side hook.
var inBoxSlots = [3]string{"context", "prelude", "postlude"}

// slotPath picks a slot's resolved path out of the hook set.
func slotPath(h config.HookSet, slot string) string {
	switch slot {
	case "context":
		return h.Context
	case "prelude":
		return h.Prelude
	case "postlude":
		return h.Postlude
	}
	return ""
}

// readInputs reads the overlay, the faber-box binary, the declared in-box
// hooks and the skills tree once. Each refusal names the template and the
// path: a hook that fails the config module's hook-file check, a skills
// source that is not a directory, a faber-box that is not a linux ELF of the
// image's architecture.
func (b *ImageBuilder) readInputs(tpl string, build config.BuildDef, in ImageInputs) (stagedInputs, error) {
	var out stagedInputs
	overlay, err := readOverlay(build)
	if err != nil {
		return out, err
	}
	out.overlay, out.hasOverlay = overlay, build.Overlay != ""
	box, err := b.readBox()
	if err != nil {
		return out, fmt.Errorf("templates.%s: %w", tpl, err)
	}
	out.box = box
	for _, slot := range inBoxSlots {
		p := slotPath(in.Hooks, slot)
		if p == "" {
			continue
		}
		if err := config.CheckHookFile(p); err != nil {
			return out, fmt.Errorf("templates.%s.hooks.%s: %s: %v", tpl, slot, p, err)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return out, fmt.Errorf("templates.%s.hooks.%s: %w", tpl, slot, err)
		}
		out.hooks = append(out.hooks, stagedHook{slot: slot, data: data})
	}
	if in.Skills != nil {
		out.hasSkills = true
		switch {
		case in.Skills.Root != "":
			files, err := readTree(in.Skills.Root, "")
			if err != nil {
				return out, fmt.Errorf("templates.%s.skills: %w", tpl, err)
			}
			out.skills = files
		default:
			for _, src := range in.Skills.Sources {
				if !safeSegment(src.Name) {
					return out, fmt.Errorf("templates.%s.skills: skill %q: unsafe name (must be a single path segment)", tpl, src.Name)
				}
				files, err := readTree(src.Dir, src.Name)
				if err != nil {
					return out, fmt.Errorf("templates.%s.skills: skill %q: %w", tpl, src.Name, err)
				}
				out.skills = append(out.skills, files...)
			}
		}
		sort.Slice(out.skills, func(i, j int) bool { return out.skills[i].rel < out.skills[j].rel })
	}
	return out, nil
}

// readBox reads the faber-box binary, proven to be a linux ELF of the image's
// architecture before a byte of it is staged. The bytes are cached by the
// file's size and modification time: every tag computation needs them, and
// the binary only changes on a faber upgrade.
func (b *ImageBuilder) readBox() ([]byte, error) {
	if b.boxBin == "" {
		return nil, errors.New("infra: no faber-box binary to bake (host.json box_bin, or faber-box next to the faber executable)")
	}
	st, err := os.Stat(b.boxBin)
	if err != nil {
		return nil, fmt.Errorf("infra: faber-box to bake: %w", err)
	}
	b.boxMu.Lock()
	defer b.boxMu.Unlock()
	if b.boxCache.data != nil && b.boxCache.size == st.Size() && b.boxCache.mtime.Equal(st.ModTime()) {
		return b.boxCache.data, nil
	}
	data, err := os.ReadFile(b.boxBin)
	if err != nil {
		return nil, fmt.Errorf("infra: faber-box to bake: %w", err)
	}
	if err := checkLinuxELF(data, runtime.GOARCH); err != nil {
		return nil, fmt.Errorf("infra: faber-box at %s: %w", b.boxBin, err)
	}
	b.boxCache = boxBytes{data: data, size: st.Size(), mtime: st.ModTime()}
	return data, nil
}

// boxBytes is the cached faber-box binary keyed by its stat.
type boxBytes struct {
	data  []byte
	size  int64
	mtime time.Time
}

// elfMachines maps the Go arch the image targets (the Linux variant of the
// host arch, as the rendered expression does) to the ELF machine it must carry.
var elfMachines = map[string]elf.Machine{
	"amd64": elf.EM_X86_64,
	"arm64": elf.EM_AARCH64,
}

// checkLinuxELF refuses anything but a linux ELF executable of the given Go
// architecture: a darwin build of faber-box on a darwin host is the mistake
// this catches before it is baked into an image that can never run it.
func checkLinuxELF(data []byte, goarch string) error {
	want, ok := elfMachines[goarch]
	if !ok {
		return fmt.Errorf("no image architecture known for host arch %s", goarch)
	}
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("not a linux ELF binary (%v); build it with CGO_ENABLED=0 GOOS=linux GOARCH=%s", err, goarch)
	}
	defer f.Close()
	if f.Machine != want {
		return fmt.Errorf("built for %s, the image needs linux/%s (%s)", f.Machine, goarch, want)
	}
	if f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX {
		return fmt.Errorf("built for %s, the image needs linux", f.OSABI)
	}
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return fmt.Errorf("not an executable (%s)", f.Type)
	}
	return nil
}

// readTree reads every regular file under dir as a staged file at
// <prefix>/<rel>. Non-regular entries (symlinks, devices) are skipped, as the
// old per-attempt copy skipped them; a dir that is not a directory is refused.
func readTree(dir, prefix string) ([]stagedFile, error) {
	// A symlinked root (~/.claude/skills is often one) is followed here:
	// WalkDir would otherwise see the link itself and walk nothing.
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	dir = real
	var out []stagedFile
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if prefix != "" {
			rel = path.Join(prefix, rel)
		}
		out = append(out, stagedFile{rel: rel, exec: info.Mode()&0o111 != 0, data: data})
		return nil
	})
	return out, err
}

// safeSegment reports whether name is a single path component safe to join
// under /faber/skills: no separator, not "." or "..", not absolute, no leading
// dot or tilde. Mirrors config.Validate's skill-name discipline so a bypassed
// validation can never write outside the staged tree.
func safeSegment(name string) bool {
	if name == "" || name == "." || name == ".." || filepath.IsAbs(name) {
		return false
	}
	if name[0] == '.' || name[0] == '~' {
		return false
	}
	return !strings.ContainsAny(name, `/\`)
}

// stageInputs writes the engine inputs beside the rendered expression, from
// the bytes the tag hashed: faber-box, hooks/<slot>, skills/<rel>. The
// expression imports them as ./ path literals.
func stageInputs(dir string, in stagedInputs) error {
	if in.box == nil {
		return nil
	}
	if err := os.WriteFile(filepath.Join(dir, stagedBoxName), in.box, 0o755); err != nil {
		return err
	}
	if len(in.hooks) > 0 {
		if err := os.Mkdir(filepath.Join(dir, stagedHooksDir), 0o755); err != nil {
			return err
		}
		for _, h := range in.hooks {
			if err := os.WriteFile(filepath.Join(dir, stagedHooksDir, h.slot), h.data, 0o755); err != nil {
				return err
			}
		}
	}
	if in.hasSkills {
		root := filepath.Join(dir, stagedSkillsDir)
		if err := os.Mkdir(root, 0o755); err != nil {
			return err
		}
		for _, f := range in.skills {
			target := filepath.Join(root, filepath.FromSlash(f.rel))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := fs.FileMode(0o644)
			if f.exec {
				mode = 0o755
			}
			if err := os.WriteFile(target, f.data, mode); err != nil {
				return err
			}
		}
	}
	return nil
}

// The staged names the rendered expression imports by relative path.
const (
	stagedBoxName   = "faber-box"
	stagedHooksDir  = "hooks"
	stagedSkillsDir = "skills"
)

// renderInputsDerivation renders the faber-inputs layer: one install line per
// declared slot, the skills copy only when a leg is declared. Slot names are
// the fixed identifiers of inBoxSlots, never user text.
func renderInputsDerivation(in stagedInputs) string {
	var sb strings.Builder
	sb.WriteString("    (pkgs.runCommand \"faber-inputs\" { } ''\n")
	sb.WriteString("      mkdir -p $out/faber/bin $out/faber/hooks\n")
	sb.WriteString("      install -m 0755 ${./" + stagedBoxName + "} $out/faber/bin/faber-box\n")
	for _, h := range in.hooks {
		fmt.Fprintf(&sb, "      install -m 0755 ${./%s/%s} $out/faber/hooks/%s\n", stagedHooksDir, h.slot, h.slot)
	}
	if in.hasSkills {
		sb.WriteString("      cp -r ${./" + stagedSkillsDir + "} $out/faber/skills\n")
	}
	sb.WriteString("    '')\n")
	return sb.String()
}
