package infra

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// nixCLI is the real NixClient over the nix binary. Both verbs always carry
// --json; decoding the payload against the rendered expression belongs to the
// caller (ImageBuilder).
type nixCLI struct {
	cli cliRunner
}

// NewNixCLI returns the real nix adapter.
func NewNixCLI(logger *slog.Logger) NixClient {
	return &nixCLI{cli: cliRunner{name: "nix", logger: ensureLogger(logger).With("adapter", "nix")}}
}

// nixBase pins the CLI mode: the modern nix command with JSON output,
// independent of the host's nix.conf.
func nixBase(verb string) []string {
	return []string{"--extra-experimental-features", "nix-command", verb, "--json"}
}

func (n *nixCLI) Eval(ctx context.Context, exprFile string, args []string) (json.RawMessage, error) {
	argv := append(nixBase("eval"), "--file", exprFile)
	argv = append(argv, args...)
	out, err := n.cli.run(ctx, argv...)
	if err != nil {
		return nil, fmt.Errorf("infra: nix eval: %w", err)
	}
	raw := json.RawMessage(bytes.TrimSpace(out))
	if !json.Valid(raw) {
		return nil, fmt.Errorf("infra: nix eval: output is not valid JSON")
	}
	return raw, nil
}

func (n *nixCLI) Build(ctx context.Context, exprFile string) ([]string, error) {
	argv := append(nixBase("build"), "--no-link", "--file", exprFile)
	out, err := n.cli.run(ctx, argv...)
	if err != nil {
		return nil, fmt.Errorf("infra: nix build: %w", err)
	}
	return parseNixBuildOut(out)
}

// parseNixBuildOut decodes nix build --json output into the built derivations'
// "out" store paths, in emission order.
func parseNixBuildOut(out []byte) ([]string, error) {
	var results []struct {
		Outputs map[string]string `json:"outputs"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &results); err != nil {
		return nil, fmt.Errorf("infra: nix build: parse --json output: %w", err)
	}
	var paths []string
	for _, r := range results {
		if p, ok := r.Outputs["out"]; ok && p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("infra: nix build: no out paths in --json output")
	}
	return paths, nil
}
