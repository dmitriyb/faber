package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HostConfig is faber's per-machine configuration: the small set of values
// that are properties of the HOST, not of a project or a role. It lives in an
// explicit file next to the role registry (host.json under faber's config
// home) so what a run will use is readable — and auditable — before anything
// runs. Faber deliberately reads none of these from the process environment:
// ambient env is assembled invisibly (shells, wrappers, parents) and is the
// wrong channel for values like the box binary path, where silent
// substitution is exactly the attack to refuse.
type HostConfig struct {
	// BoxBin is the faber-box sequencer binary bind-mounted into every box.
	// Empty means the default: next to the faber executable. Must be absolute
	// when set (docker reads a relative -v host path as a named volume).
	BoxBin string `json:"box_bin,omitempty"`

	// AgentSocketGroup, when non-empty, is passed as --group-add on every box
	// so the box's non-root user can reach the forwarded agent socket on
	// platforms whose VM mislabels its ownership (macOS Docker Desktop;
	// typically "0"). Use a numeric gid — a group name must exist inside the
	// box image. Empty (every Linux host) emits nothing.
	AgentSocketGroup string `json:"agent_socket_group,omitempty"`

	// StateDir overrides where run journals and the image manifest live
	// (default .faber beside the working directory).
	StateDir string `json:"state_dir,omitempty"`
}

// HostConfigPath returns host.json under faber's config home, sibling of
// roles.json: $XDG_CONFIG_HOME/faber/host.json when XDG_CONFIG_HOME is set
// and absolute, else ~/.config/faber/host.json.
func HostConfigPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "faber", "host.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "faber", "host.json")
	}
	return filepath.Join(home, ".config", "faber", "host.json")
}

// LoadHostConfig reads and validates the host config. A missing file is an
// empty config (every value defaults) — not an error. A present-but-malformed
// file is a hard error: faber never half-reads its host configuration. The
// decode is strict (unknown keys rejected) so a typo'd knob cannot be
// silently ignored.
func LoadHostConfig(path string) (HostConfig, error) {
	var hc HostConfig
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return hc, nil
		}
		return hc, fmt.Errorf("read host config %s: %w", path, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&hc); err != nil {
		return hc, fmt.Errorf("parse host config %s: %w", path, err)
	}
	if dec.More() {
		return hc, fmt.Errorf("parse host config %s: trailing content after the config object", path)
	}
	if hc.BoxBin != "" && !filepath.IsAbs(hc.BoxBin) {
		return hc, fmt.Errorf("host config %s: box_bin %q must be an absolute path", path, hc.BoxBin)
	}
	return hc, nil
}

// Describe renders the effective host config as one human line for the
// startup log, so every run states what it is using and where it came from.
func (hc HostConfig) Describe(path string) string {
	var parts []string
	if hc.BoxBin != "" {
		parts = append(parts, "box_bin="+hc.BoxBin)
	}
	if hc.AgentSocketGroup != "" {
		parts = append(parts, "agent_socket_group="+hc.AgentSocketGroup)
	}
	if hc.StateDir != "" {
		parts = append(parts, "state_dir="+hc.StateDir)
	}
	if len(parts) == 0 {
		return path + ": absent or empty (all defaults)"
	}
	return path + ": " + strings.Join(parts, " ")
}
