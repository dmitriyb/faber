package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/agent/contract"
	"github.com/dmitriyb/faber/config"
)

func testTemplate() *config.ResolvedTemplate {
	return &config.ResolvedTemplate{
		Name:     "tpl-a",
		Identity: "role-a",
		Skill:    "skill-a",
		Inputs: map[string]config.ParamDef{
			"alpha": {Type: "string", Required: true},
			"beta":  {Type: "int"},
		},
		Output: map[string]config.FieldDef{
			"verdict": {Type: "string", Required: true, Enum: []string{"ok", "changes"}},
		},
		Env:     map[string]string{"TOOL_HOME": "/opt/tool"},
		Volumes: map[string]string{"/host/cache": "/cache"},
	}
}

func validSpec() BoxSpec {
	return BoxSpec{
		RunID:       "run1",
		NodeID:      "task/implement",
		Attempt:     2,
		Template:    testTemplate(),
		Image:       "faber/tpl-a:abc",
		Inputs:      map[string]string{"alpha": "v1"},
		ResultDir:   "/host/results/task-implement",
		EntryBinary: "/host/bin/faber-box",
		ContextHook: "/host/hooks/ctx",
		PreludeHook: "/host/hooks/pre",
		AgentCLI:    "agent-cli",
	}
}

// Verifies 93ba0858d75f: the host half emits the full box env contract —
// skill, identity, dirs, output schema, required inputs, attempt, and one
// FABER_INPUT_* per bound slot — plus the engine mounts and the fixed entry.
func TestBuildRunSpecContract(t *testing.T) {
	rs, err := BuildRunSpec(validSpec())
	if err != nil {
		t.Fatal(err)
	}
	wantEnv := map[string]string{
		contract.EnvSkill:          "skill-a",
		contract.EnvIdentity:       "role-a",
		contract.EnvAgentCLI:       "agent-cli",
		contract.EnvResultDir:      contract.ContainerResultDir,
		contract.EnvBundleDir:      contract.ContainerBundleDir,
		contract.EnvRequiredInputs: "alpha",
		contract.EnvAttempt:        "2",
		"FABER_INPUT_ALPHA":        "v1",
		"TOOL_HOME":                "/opt/tool",
	}
	for key, want := range wantEnv {
		if rs.Env[key] != want {
			t.Errorf("env[%s] = %q, want %q", key, rs.Env[key], want)
		}
	}
	if !strings.Contains(rs.Env[contract.EnvOutputSchema], `"enum":["ok","changes"]`) {
		t.Errorf("output schema env = %q", rs.Env[contract.EnvOutputSchema])
	}
	if fmt.Sprint(rs.Entry) != fmt.Sprintf("[%s]", contract.ContainerEntry) {
		t.Errorf("entry = %v", rs.Entry)
	}
	if want := ContainerName("run1", "task/implement", 2); rs.Name != want {
		t.Errorf("container name = %q, want %q", rs.Name, want)
	}
	var mounts []string
	for _, m := range rs.Mounts {
		ro := ""
		if m.ReadOnly {
			ro = ":ro"
		}
		mounts = append(mounts, m.Host+":"+m.Container+ro)
	}
	want := []string{
		"/host/results/task-implement:" + contract.ContainerResultDir,
		"/host/bin/faber-box:" + contract.ContainerEntry + ":ro",
		"/host/hooks/ctx:" + contract.ContainerHooksDir + "/" + contract.HookContext + ":ro",
		"/host/hooks/pre:" + contract.ContainerHooksDir + "/" + contract.HookPrelude + ":ro",
		"/host/cache:/cache",
	}
	if fmt.Sprint(mounts) != fmt.Sprint(want) {
		t.Errorf("mounts = %v, want %v", mounts, want)
	}
	// Optional pass-throughs stay absent when unset.
	for _, key := range []string{contract.EnvEffort, contract.EnvMaxBudget, contract.EnvExtraInstruction} {
		if _, ok := rs.Env[key]; ok {
			t.Errorf("env unexpectedly carries %s", key)
		}
	}
}

// Verifies ae434449cac9: the agent CLI is opaque user config with no vendor
// default — absent everywhere it is a build error, and the template env is
// an accepted source.
func TestBuildRunSpecAgentCLIRequired(t *testing.T) {
	spec := validSpec()
	spec.AgentCLI = ""
	if _, err := BuildRunSpec(spec); err == nil || !strings.Contains(err.Error(), "no vendor default") {
		t.Fatalf("err = %v, want missing agent cli", err)
	}
	spec.Template.Env[contract.EnvAgentCLI] = "cli-from-template"
	rs, err := BuildRunSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Env[contract.EnvAgentCLI] != "cli-from-template" {
		t.Fatalf("agent cli = %q", rs.Env[contract.EnvAgentCLI])
	}
}

// Verifies 93ba0858d75f: violations are collected — an undeclared input and
// a missing required input surface together, before any container exists.
func TestBuildRunSpecCollectsViolations(t *testing.T) {
	spec := validSpec()
	spec.Inputs = map[string]string{"gamma": "x"} // undeclared; alpha missing
	_, err := BuildRunSpec(spec)
	if err == nil {
		t.Fatal("want error")
	}
	for _, part := range []string{`"gamma"`, `"alpha"`} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("err %q misses %s", err, part)
		}
	}
}

// Verifies 93ba0858d75f: container names are deterministic per attempt and
// injective over (run-id, node-id) — slugging alone would collide "task/x"
// with "task-x" and blur the run/node boundary.
func TestContainerNameDeterministicAndInjective(t *testing.T) {
	a := ContainerName("Run 1", "task/review-cycle@2/fix", 1)
	b := ContainerName("Run 1", "task/review-cycle@2/fix", 1)
	if a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "faber-run-1-task-review-cycle-2-fix-") || !strings.HasSuffix(a, "-a1") {
		t.Fatalf("name = %q", a)
	}
	collisions := [][2][2]string{
		{{"run1", "task/x"}, {"run1", "task-x"}},   // slug collision
		{{"run-a", "b/node"}, {"run", "a-b/node"}}, // run/node boundary ambiguity
	}
	for _, pair := range collisions {
		x := ContainerName(pair[0][0], pair[0][1], 1)
		y := ContainerName(pair[1][0], pair[1][1], 1)
		if x == y {
			t.Errorf("ContainerName%v == ContainerName%v == %q", pair[0], pair[1], x)
		}
	}
}

// Verifies 93ba0858d75f: template env may not set engine- or security-owned
// names — FABER_AGENT_CLI is the one documented exception — or user config
// could redirect hooks, enable TOFU, or point the box at an arbitrary
// remote. All offenders are collected.
func TestBuildRunSpecRejectsReservedTemplateEnv(t *testing.T) {
	spec := validSpec()
	spec.Template.Env = map[string]string{
		"TOOL_HOME":           "/opt/tool", // legitimate user env
		contract.EnvAgentCLI:  "agent-cli", // the documented exception
		"FABER_REMOTE_URL":    "ssh://evil/x.git",
		"FABER_HOST_KEY_TOFU": "1",
		"FABER_HOOKS_DIR":     "/workspace/repo-a/hooks",
		"SSH_AUTH_SOCK":       "/elsewhere",
		"FABER_SERVICE_X_URL": "http://evil",
		contract.EnvResultDir: "/elsewhere",
		contract.EnvAttempt:   "9",
	}
	_, err := BuildRunSpec(spec)
	if err == nil {
		t.Fatal("want error")
	}
	for _, key := range []string{"FABER_REMOTE_URL", "FABER_HOST_KEY_TOFU", "FABER_HOOKS_DIR", "SSH_AUTH_SOCK", "FABER_SERVICE_X_URL", contract.EnvResultDir, contract.EnvAttempt} {
		if !strings.Contains(err.Error(), fmt.Sprintf("%q", key)) {
			t.Errorf("err misses reserved key %s:\n%v", key, err)
		}
	}
	for _, key := range []string{"TOOL_HOME", contract.EnvAgentCLI} {
		if strings.Contains(err.Error(), fmt.Sprintf("%q", key)) {
			t.Errorf("err wrongly flags allowed key %s:\n%v", key, err)
		}
	}
}

// Verifies 93ba0858d75f: template volumes may not shadow (or be shadowed
// through) the engine and binding mounts — last-mount-wins would sever the
// result channel or substitute hooks, secrets, or the forwarded socket.
func TestBuildRunSpecRejectsShadowingVolumes(t *testing.T) {
	bad := []string{
		"/faber/result",
		"/faber",
		"/faber/hooks/context",
		"/faber/bin",
		"/run/secrets",
		"/run/secrets/service-token",
		"/ssh-agent",
		"/workspace",
		"/workspace/repo-a",
		"/",
	}
	for _, container := range bad {
		t.Run(container, func(t *testing.T) {
			spec := validSpec()
			spec.Template.Volumes = map[string]string{"/host/x": container}
			if _, err := BuildRunSpec(spec); err == nil || !strings.Contains(err.Error(), "reserved container path") {
				t.Fatalf("volume %q: err = %v, want reserved-path violation", container, err)
			}
		})
	}
	spec := validSpec()
	spec.Template.Volumes = map[string]string{"/host/cache": "/cache"}
	if _, err := BuildRunSpec(spec); err != nil {
		t.Fatalf("a non-reserved volume must pass: %v", err)
	}
}
