package infra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

func (d *dockerCLI) ContainerRun(ctx context.Context, args []string, output io.Writer) (int, error) {
	code, err := d.cli.runStreaming(ctx, output, args...)
	if err != nil {
		return code, err
	}
	if code == dockerDaemonExit {
		// The docker client itself failed before the box command ran; that is
		// an actuation failure, not step data. Its diagnostics went to output.
		return code, &ExecError{Cmd: "docker", Args: args, ExitCode: code,
			Err: errors.New("docker run failed before the container command ran")}
	}
	return code, nil
}

func (d *dockerCLI) Kill(ctx context.Context, name string) error {
	if _, err := d.cli.run(ctx, "kill", name); err != nil {
		return fmt.Errorf("infra: docker kill %s: %w", name, err)
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
