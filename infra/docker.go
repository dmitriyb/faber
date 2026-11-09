package infra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// dockerDaemonExit is docker's reserved exit code for a failure of the docker
// client/daemon itself (daemon unreachable, image missing, bad flag) — the
// container command never ran, so it is an actuation error, not step data.
const dockerDaemonExit = 125

// dockerCLI is the real DockerClient over the docker binary.
type dockerCLI struct {
	cli cliRunner
}

// NewDockerCLI returns the real docker adapter. All query verbs pin their
// output format; no verb scrapes free text.
func NewDockerCLI(logger *slog.Logger) DockerClient {
	return &dockerCLI{cli: cliRunner{name: "docker", logger: ensureLogger(logger).With("adapter", "docker")}}
}

func (d *dockerCLI) ImageExists(ctx context.Context, tag string) (bool, error) {
	out, err := d.cli.run(ctx, "image", "inspect", "--format", "{{json .Id}}", tag)
	if isNotFound(err, "No such image") {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("infra: docker image inspect %s: %w", tag, err)
	}
	id, perr := parseJSONString(out)
	if perr != nil {
		return false, fmt.Errorf("infra: docker image inspect %s: parse: %w", tag, perr)
	}
	return id != "", nil
}

func (d *dockerCLI) NetworkExists(ctx context.Context, name string) (bool, error) {
	out, err := d.cli.run(ctx, "network", "inspect", "--format", "{{json .Name}}", name)
	if isNotFound(err, "No such network", "network "+name+" not found") {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("infra: docker network inspect %s: %w", name, err)
	}
	got, perr := parseJSONString(out)
	if perr != nil {
		return false, fmt.Errorf("infra: docker network inspect %s: parse: %w", name, perr)
	}
	return got != "", nil
}

func (d *dockerCLI) Load(ctx context.Context, tarball string) (string, error) {
	out, err := d.cli.run(ctx, "load", "--input", tarball)
	if err != nil {
		return "", fmt.Errorf("infra: docker load: %w", err)
	}
	tag, perr := parseLoadedTag(out)
	if perr != nil {
		return "", perr
	}
	return tag, nil
}

func (d *dockerCLI) ContainerRun(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	code, err := d.cli.runStreaming(ctx, stdout, stderr, stdin, args...)
	if err != nil {
		return code, err
	}
	if code == dockerDaemonExit {
		// The docker client itself failed before the box command ran; that is
		// an actuation failure, not step data. Its diagnostics went to stderr.
		return code, &ExecError{Cmd: "docker", Args: args, ExitCode: code,
			Err: errors.New("docker run failed before the container command ran")}
	}
	return code, nil
}

func (d *dockerCLI) Exec(ctx context.Context, name string, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	args := execArgs(name, argv, stdin != nil)
	code, err := d.cli.runStreaming(ctx, stdout, stderr, stdin, args...)
	if err != nil {
		return code, err
	}
	if code == dockerDaemonExit {
		return code, &ExecError{Cmd: "docker", Args: args, ExitCode: code,
			Err: errors.New("docker exec failed before the command ran")}
	}
	return code, nil
}

// ExecTerminal attaches the operator's own terminal: the one adapter verb
// that touches the process's standard streams, so it lives here beside the
// other docker verbs rather than in the wiring. The exec's error is returned
// verbatim; the operator saw whatever it printed.
func (d *dockerCLI) ExecTerminal(ctx context.Context, name string, argv []string, env map[string]string, user, workdir string) error {
	args := execTerminalArgs(name, argv, env, user, workdir)
	cmd := exec.CommandContext(ctx, d.cli.name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	d.cli.logger.DebugContext(ctx, "exec", "cmd", d.cli.name, "args", args)
	return cmd.Run()
}

// execArgs is the docker exec argv: -i exactly when a stdin is attached.
func execArgs(name string, argv []string, interactive bool) []string {
	args := []string{"exec"}
	if interactive {
		args = append(args, "-i")
	}
	args = append(args, name)
	return append(args, argv...)
}

// execTerminalArgs is the terminal-attached exec argv: -it, the user and
// workdir when given, the env sorted, then the container and its argv.
func execTerminalArgs(name string, argv []string, env map[string]string, user, workdir string) []string {
	args := []string{"exec", "-it"}
	if user != "" {
		args = append(args, "-u", user)
	}
	if workdir != "" {
		args = append(args, "-w", workdir)
	}
	for _, k := range slices.Sorted(maps.Keys(env)) {
		args = append(args, "-e", k+"="+env[k])
	}
	args = append(args, name)
	return append(args, argv...)
}

// Running queries the container's inspect state. `docker inspect` on an
// absent name says "No such object"; `container inspect` keeps the
// container wording, so both markers are classified as absent.
func (d *dockerCLI) Running(ctx context.Context, name string) (bool, error) {
	out, err := d.cli.run(ctx, "container", "inspect", "--format", "{{json .State.Running}}", name)
	if isNotFound(err, "No such container", "No such object") {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("infra: docker container inspect %s: %w", name, err)
	}
	running, perr := parseJSONBool(out)
	if perr != nil {
		return false, fmt.Errorf("infra: docker container inspect %s: parse: %w", name, perr)
	}
	return running, nil
}

func (d *dockerCLI) Kill(ctx context.Context, name string) error {
	if _, err := d.cli.run(ctx, "kill", name); err != nil {
		return fmt.Errorf("infra: docker kill %s: %w", name, err)
	}
	return nil
}

func (d *dockerCLI) Remove(ctx context.Context, name string) error {
	_, err := d.cli.run(ctx, "rm", "--force", name)
	// Absent and already-being-removed (--rm racing this eviction) both mean
	// the name is (about to be) free — success for an idempotent eviction.
	if err != nil && !isNotFound(err, "No such container", "is already in progress") {
		return fmt.Errorf("infra: docker rm %s: %w", name, err)
	}
	return nil
}

// isNotFound classifies a docker inspect failure as "object absent" rather
// than an actuation error. This inspects the error's bounded stderr tail —
// error classification, not output parsing; the success path stays structured.
func isNotFound(err error, markers ...string) bool {
	var xerr *ExecError
	if !errors.As(err, &xerr) {
		return false
	}
	for _, m := range markers {
		if strings.Contains(xerr.Stderr, m) {
			return true
		}
	}
	return false
}

// isNoSuchContainer reports whether a Kill failed only because the container
// is already gone (--rm completed first).
func isNoSuchContainer(err error) bool {
	return isNotFound(err, "No such container")
}

// VolumeList asks the daemon for the names under prefix. The daemon's name
// filter matches a substring, so the prefix is re-checked on the result.
func (d *dockerCLI) VolumeList(ctx context.Context, prefix string) ([]string, error) {
	out, err := d.cli.run(ctx, "volume", "ls", "--filter", "name="+prefix, "--format", "{{json .Name}}")
	if err != nil {
		return nil, fmt.Errorf("infra: docker volume ls: %w", err)
	}
	var names []string
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		name, perr := parseJSONString(line)
		if perr != nil {
			return nil, fmt.Errorf("infra: docker volume ls: parse: %w", perr)
		}
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	return names, nil
}

func (d *dockerCLI) VolumeRemove(ctx context.Context, name string) error {
	_, err := d.cli.run(ctx, "volume", "rm", name)
	if err != nil && !isNotFound(err, "No such volume", "no such volume") {
		return fmt.Errorf("infra: docker volume rm %s: %w", name, err)
	}
	return nil
}

// parseJSONBool decodes the single JSON boolean a pinned
// --format '{{json .State.Running}}' emits.
func parseJSONBool(out []byte) (bool, error) {
	var b bool
	if err := json.Unmarshal(bytes.TrimSpace(out), &b); err != nil {
		return false, err
	}
	return b, nil
}

// parseJSONString decodes the single JSON string a pinned
// --format '{{json .Field}}' emits.
func parseJSONString(out []byte) (string, error) {
	var s string
	if err := json.Unmarshal(bytes.TrimSpace(out), &s); err != nil {
		return "", fmt.Errorf("decode %q: %w", string(bytes.TrimSpace(out)), err)
	}
	return s, nil
}

// loadedTagPrefix is the fixed prefix of docker load's result line — the one
// deliberate non-JSON parse in the docker adapter, byte-stable and handled in
// exactly this function.
const loadedTagPrefix = "Loaded image: "

// parseLoadedTag extracts the tag from docker load output. Output that names
// no tagged image (e.g. only "Loaded image ID: …") is a hard error: the build
// pipeline requires the loaded tag to compare against the computed one.
func parseLoadedTag(out []byte) (string, error) {
	for _, line := range strings.Split(string(out), "\n") {
		if tag, ok := strings.CutPrefix(strings.TrimSpace(line), loadedTagPrefix); ok && tag != "" {
			return tag, nil
		}
	}
	return "", fmt.Errorf("infra: docker load: no %q line in output", strings.TrimSpace(loadedTagPrefix))
}
