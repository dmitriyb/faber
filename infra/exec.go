// Package infra is faber's actuation layer: typed subprocess adapters for the
// docker, git, and nix CLIs plus opaque user commands, the Nix image build
// pipeline that turns a template's pinned package list into an immutable
// docker image, and the container-run primitive that assembles a docker run
// argv from engine mounts, environment, and the security module's ordered
// binding contributions.
//
// This package is the only place os/exec appears in faber. Every verb is
// reachable through one of four small interfaces (DockerClient, GitClient,
// NixClient, CommandRunner), each taking a context.Context and fakeable in
// tests; the real implementations use the tools' structured output modes
// (docker --format json, git plumbing, nix --json) — never free-text stdout
// scraping.
package infra

import (
	"context"
	"encoding/json"
	"io"
)

// DockerClient is the typed docker CLI seam. ImageExists and NetworkExists
// query with pinned --format json; Load parses the single fixed-format
// "Loaded image:" result line; ContainerRun executes a pre-assembled argv
// (only ContainerRunner ever constructs one) streaming combined output to the
// writer; Kill stops a container by its deterministic name.
type DockerClient interface {
	ImageExists(ctx context.Context, tag string) (bool, error)
	Load(ctx context.Context, tarball string) (string, error) // returns the loaded tag
	NetworkExists(ctx context.Context, name string) (bool, error)
	ContainerRun(ctx context.Context, args []string, output io.Writer) (int, error)
	Kill(ctx context.Context, name string) error
}

// GitClient is the typed git seam. Only plumbing verbs with byte-stable
// output live here.
type GitClient interface {
	// LsRemote resolves ref on the remote at url to a commit sha, or
	// ErrRefAbsent when the remote has no such ref.
	LsRemote(ctx context.Context, url, ref string) (string, error)
}

// NixClient is the typed nix seam. Both verbs always carry --json; the raw
// messages are decoded by the caller against the expression it rendered.
type NixClient interface {
	Eval(ctx context.Context, exprFile string, args []string) (json.RawMessage, error)
	Build(ctx context.Context, exprFile string) ([]string, error) // store out paths
}

// CommandRunner is the opaque-command seam: credential resolvers, generate
// data-source commands, and host-side cleanup hooks. It is deliberately dumber
// than the other adapters — it returns raw bytes and lets the caller type
// them. Because stdout may be a credential, no CommandRunner error or log
// record ever carries stdout bytes or the argument list.
type CommandRunner interface {
	Run(ctx context.Context, spec CmdSpec) (CmdResult, error)
}

// CmdSpec describes one opaque user-command invocation. The command file is
// executed directly with the declared args — never through a shell.
type CmdSpec struct {
	Path  string // the user's command file, executed directly (no shell)
	Args  []string
	Stdin []byte
	Env   []string // appended to a minimal base env, never the full host env
	Dir   string
}

// CmdResult is a finished user-command invocation. Stdout is typed by the
// caller and may be secret — it is never logged or embedded in errors here.
type CmdResult struct {
	Stdout   []byte // typed by the caller; may be secret — never logged here
	Stderr   []byte // bounded tail
	ExitCode int
}
