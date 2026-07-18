package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// AssemblyViolation is a cross-file rule broken during Assemble — either a
// duplicate library key across files or a substrate key on a non-root file.
// These need per-file provenance the merged Config no longer carries, so they
// are recorded here and surfaced through Validate alongside the schema errors.
type AssemblyViolation struct {
	Path string // field-ish path or the offending file
	Msg  string
}

// Load reads the root project file, resolves and union-merges its transitive
// include: closure into one assembled *Config, and rewrites every declared file
// path to be relative to its declaring file. It runs no schema validation, so
// callers report as much as possible from a partially broken file; run Validate
// on the result, passing back the returned violations. Load is Assemble kept as
// the public name.
func Load(path string) (*Config, []AssemblyViolation, error) {
	return Assemble(path)
}

// Assemble folds the include DAG rooted at path into one Config. Only two
// conditions hard-stop (they cannot yield a Config): an unreadable/unparseable
// file and an include cycle. Everything else — duplicate library keys across
// files, substrate on a non-root file — is recorded as an AssemblyViolation and
// returned for Validate to collect. A single-file config assembles to itself
// with an empty violation slice.
func Assemble(path string) (*Config, []AssemblyViolation, error) {
	a := &assembler{
		result: &Config{},
		origin: map[string]string{},
		seen:   map[string]bool{},
	}
	if err := a.assemble(path, true, nil); err != nil {
		return nil, nil, err
	}
	return a.result, a.viols, nil
}

// assembler carries the shared assembly state: the accumulating merged Config,
// per-library-key provenance for duplicate detection, the diamond/cycle
// bookkeeping, and the recorded violations.
type assembler struct {
	result *Config
	origin map[string]string // "<lib>/<name>" -> the file that first declared it
	seen   map[string]bool   // canonical file identity -> already merged (diamond)
	viols  []AssemblyViolation
}

// assemble reads one already-resolved file path, merges its libraries into the
// result, and recurses into its includes. isRoot honors the substrate; stack is
// the current DFS include chain (readable paths) for cycle detection.
func (a *assembler) assemble(readPath string, isRoot bool, stack []string) error {
	id := identity(readPath)
	for _, s := range stack {
		if identity(s) == id {
			chain := append(append([]string(nil), stack...), readPath)
			return fmt.Errorf("config: include cycle: %s", strings.Join(chain, " -> "))
		}
	}
	if a.seen[id] {
		return nil // diamond: this file was already merged once
	}
	a.seen[id] = true

	data, err := os.ReadFile(readPath)
	if err != nil {
		return fmt.Errorf("config: read %s: %w", readPath, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("config: parse %s: %w", readPath, err)
	}
	dir := filepath.Dir(readPath)
	// Declarer-relative path resolution rewrites an included file's declared
	// paths to resolve against its OWN directory — the multi-file-composition
	// gotcha the include: directive would otherwise introduce (a library file's
	// ./overlay.nix must mean the library's dir, not the root's/CWD). The root
	// project file is the CWD reference and is left verbatim, so every existing
	// single-file config desugars to a byte-identical ResolvedTemplate.
	if !isRoot {
		resolvePaths(&cfg, dir)
	}

	if isRoot {
		// Substrate is honored only in the root project file.
		a.result.Version = cfg.Version
		a.result.Network = cfg.Network
		a.result.Remote = cfg.Remote
		a.result.Credentials = cfg.Credentials
		a.result.Identities = cfg.Identities
	} else if cfg.hasSubstrate() {
		a.viols = append(a.viols, AssemblyViolation{
			Path: readPath,
			Msg:  "included files may only contribute libraries (substrate keys are honored only in the root project file)",
		})
	}

	// Union-merge the five named libraries with per-key provenance.
	mergeLib(a, readPath, "images", cfg.Images, &a.result.Images)
	mergeLib(a, readPath, "skills", cfg.Skills, &a.result.Skills)
	mergeLib(a, readPath, "hooks", cfg.Hooks, &a.result.Hooks)
	mergeLib(a, readPath, "templates", cfg.Templates, &a.result.Templates)
	mergeLib(a, readPath, "workflows", cfg.Workflows, &a.result.Workflows)

	childStack := append(append([]string(nil), stack...), readPath)
	for _, inc := range cfg.Include {
		if err := a.assemble(joinRel(dir, inc), false, childStack); err != nil {
			return err
		}
	}
	return nil
}

// mergeLib unions one named library from src into dst, recording a duplicate
// violation (naming both files) when a key was already declared by a different
// file — no silent last-wins.
func mergeLib[V any](a *assembler, file, lib string, src map[string]V, dst *map[string]V) {
	if len(src) == 0 {
		return
	}
	if *dst == nil {
		*dst = map[string]V{}
	}
	for _, k := range sortedKeys(src) {
		key := lib + "/" + k
		if prev, ok := a.origin[key]; ok {
			a.viols = append(a.viols, AssemblyViolation{
				Path: lib + "." + k,
				Msg:  fmt.Sprintf("duplicate library key: defined in both %s and %s", prev, file),
			})
			continue
		}
		a.origin[key] = file
		(*dst)[k] = src[k]
	}
}

// resolvePaths rewrites every file path a config declares to be relative to dir
// (the declaring file's directory): image/skill/hook library paths, the inline
// build.overlay / skills.dir / hooks.* path forms, and the substrate paths
// identities.*.key, credentials.resolver, remote.host_key_file. Bare hook names
// (not paths) are left untouched for the library resolver. The rule is uniform:
// every declared file path becomes declarer-relative, so a multi-file assembly
// resolves each path against its own file, not the process CWD.
func resolvePaths(cfg *Config, dir string) {
	for name, img := range cfg.Images {
		img.Overlay = joinRel(dir, img.Overlay)
		cfg.Images[name] = img
	}
	for name, sk := range cfg.Skills {
		sk.Dir = joinRel(dir, sk.Dir)
		cfg.Skills[name] = sk
	}
	for name, h := range cfg.Hooks {
		h.Path = joinRel(dir, h.Path)
		cfg.Hooks[name] = h
	}
	for name, t := range cfg.Templates {
		if t.Build != nil {
			b := *t.Build
			b.Overlay = joinRel(dir, b.Overlay)
			t.Build = &b
		}
		if t.Skills.Inline != nil {
			in := *t.Skills.Inline
			in.Dir = joinRel(dir, in.Dir)
			t.Skills.Inline = &in
		}
		t.Hooks.Context = resolveHookField(dir, t.Hooks.Context)
		t.Hooks.Prelude = resolveHookField(dir, t.Hooks.Prelude)
		t.Hooks.OnFailure = resolveHookField(dir, t.Hooks.OnFailure)
		cfg.Templates[name] = t
	}
	for name, id := range cfg.Identities {
		id.Key = joinRel(dir, id.Key)
		cfg.Identities[name] = id
	}
	cfg.Credentials.Resolver = joinRel(dir, cfg.Credentials.Resolver)
	cfg.Remote.HostKeyFile = joinRel(dir, cfg.Remote.HostKeyFile)
}

// resolveHookField rewrites a hooks.<field> value only when it is a path; a bare
// hook name is a library reference and must survive verbatim for the resolver.
func resolveHookField(dir, v string) string {
	if !isPath(v) {
		return v
	}
	return joinRel(dir, v)
}

// isPath reports whether a hooks.<field> value is an inline path rather than a
// bare hook-library name. Per the schema disambiguation it is a path iff it
// contains a path separator or begins with '.', '~', or '/'. POSIX-oriented, to
// match faber's Linux-container host convention.
func isPath(v string) bool {
	if v == "" {
		return false
	}
	switch v[0] {
	case '.', '~', '/':
		return true
	}
	return strings.ContainsRune(v, '/')
}

// joinRel resolves p against dir. Empty and already-absolute paths pass through
// unchanged; a relative path is joined (and cleaned) onto dir.
func joinRel(dir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

// identity is the canonical key for cycle and diamond detection: the cleaned,
// symlink-resolved absolute form of a file path, so two spellings of the same
// file — including a direct path and a symlink alias reaching it — compare equal
// and a legitimate diamond include merges once instead of being rejected as a
// duplicate. EvalSymlinks resolves the alias; on error (e.g. the file does not
// yet exist) fall back to Abs, preserving the prior behavior.
func identity(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}

// hasSubstrate reports whether a config fragment sets any substrate key —
// version, network, remote, credentials, or identities — which is honored only
// in the root project file.
func (c *Config) hasSubstrate() bool {
	n := c.Network
	netSet := n.Name != "" || n.Proxy != "" || len(n.NoProxy) > 0 || n.Nftables
	r := c.Remote
	remoteSet := r.URL != "" || r.HostKeyFile != "" || r.TOFU
	cr := c.Credentials
	credSet := cr.Resolver != "" || len(cr.Services) > 0
	return c.Version != 0 || netSet || remoteSet || credSet || len(c.Identities) > 0
}

// sortedKeys returns the map's keys in sorted order. Every map iteration in
// this package goes through it so that error reporting and IR emission are
// deterministic.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
