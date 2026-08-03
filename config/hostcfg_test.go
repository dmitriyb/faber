package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeHostCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "host.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Verifies the explicit-host-inputs proposal (2026-08-03): the host config is
// an explicit file — absent means all defaults, malformed or half-understood
// refuses the invocation, and no value ever comes from the environment.
func TestLoadHostConfig(t *testing.T) {
	hc, err := LoadHostConfig(filepath.Join(t.TempDir(), "host.json"))
	if err != nil || hc != (HostConfig{}) {
		t.Fatalf("missing file: hc=%+v err=%v, want empty config and nil error", hc, err)
	}

	hc, err = LoadHostConfig(writeHostCfg(t,
		`{"box_bin":"/opt/faber/faber-box","agent_socket_group":"0","state_dir":"/tmp/faber-state"}`))
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if hc.BoxBin != "/opt/faber/faber-box" || hc.AgentSocketGroup != "0" || hc.StateDir != "/tmp/faber-state" {
		t.Fatalf("decoded %+v", hc)
	}

	for name, body := range map[string]string{
		"unknown key":      `{"box_bin":"/x","surprise":1}`,
		"relative box_bin": `{"box_bin":"bin/faber-box"}`,
		"trailing content": `{"state_dir":"/s"}{"box_bin":"/x"}`,
		"malformed":        `{`,
	} {
		if _, err := LoadHostConfig(writeHostCfg(t, body)); err == nil {
			t.Errorf("%s: config should be refused: %s", name, body)
		}
	}
}

func TestHostConfigDescribe(t *testing.T) {
	if got := (HostConfig{}).Describe("/p/host.json"); !strings.Contains(got, "absent or empty") {
		t.Fatalf("empty describe = %q", got)
	}
	got := HostConfig{AgentSocketGroup: "0"}.Describe("/p/host.json")
	if !strings.Contains(got, "agent_socket_group=0") || !strings.Contains(got, "/p/host.json") {
		t.Fatalf("describe = %q", got)
	}
}
