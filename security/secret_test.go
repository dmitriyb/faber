package security

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/infra"
)

// Verifies 0c5bc0f678b7: a Secret formatted via %s, %v, %+v, %#v, %q, error
// wrapping, json.Marshal (bare and nested), text marshalling, and slog output
// yields "[redacted]" in every case — test scenario 6.
func TestSecretRedactsEveryFormattingPath(t *testing.T) {
	const raw = "hunter2-super-secret"
	s := NewSecret([]byte(raw))

	assertRedacted := func(name, got string) {
		t.Helper()
		if strings.Contains(got, raw) {
			t.Fatalf("%s leaked the secret: %q", name, got)
		}
		if !strings.Contains(got, redacted) {
			t.Fatalf("%s did not redact: %q", name, got)
		}
	}

	for _, verb := range []string{"%s", "%v", "%+v", "%#v", "%q", "%d"} {
		assertRedacted("verb "+verb, fmt.Sprintf(verb, s))
	}
	assertRedacted("error wrapping", fmt.Errorf("resolver said: %v", s).Error())
	assertRedacted("nested struct %+v", fmt.Sprintf("%+v", struct{ Tok Secret }{s}))
	assertRedacted("nested struct %#v", fmt.Sprintf("%#v", struct{ Tok Secret }{s}))

	j, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	assertRedacted("json.Marshal", string(j))
	j, err = json.Marshal(map[string]Secret{"token": s})
	if err != nil {
		t.Fatalf("json.Marshal nested: %v", err)
	}
	assertRedacted("json.Marshal nested", string(j))

	txt, err := s.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	assertRedacted("MarshalText", string(txt))

	var logBuf bytes.Buffer
	slog.New(slog.NewJSONHandler(&logBuf, nil)).Info("resolved", "token", s)
	assertRedacted("slog", logBuf.String())

	if got := string(s.reveal()); got != raw {
		t.Fatalf("reveal: want %q, got %q", raw, got)
	}
}

// Verifies 0c5bc0f678b7: the production resolver invokes the opaque user
// command as argv [resolver, service] via infra's CommandRunner, trims the
// trailing newline, and types stdout as a Secret.
func TestExecResolverInvokesUserCommand(t *testing.T) {
	runner := &fakeRunner{res: infra.CmdResult{Stdout: []byte("tok-abc\n")}}
	r := NewExecResolver("./hooks/get-token", runner)
	tok, err := r.GetToken(context.Background(), "agent-api")
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if got := string(tok.reveal()); got != "tok-abc" {
		t.Fatalf("token: want %q, got %q", "tok-abc", got)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("want 1 resolver invocation, got %d", len(runner.calls))
	}
	call := runner.calls[0]
	if call.Path != "./hooks/get-token" || len(call.Args) != 1 || call.Args[0] != "agent-api" {
		t.Fatalf("argv: want [./hooks/get-token agent-api], got %q %q", call.Path, call.Args)
	}
	if len(call.Stdin) != 0 {
		t.Fatalf("stdin must be closed, got %d bytes", len(call.Stdin))
	}
}

// Verifies 0c5bc0f678b7: a resolver failure's error names the service and
// carries no stdout content, and an empty stdout is an error — test
// scenario 6's second half.
func TestExecResolverFailuresNameServiceNotOutput(t *testing.T) {
	t.Run("non-zero exit", func(t *testing.T) {
		// Emulate infra's contract: stdout captured but never in the error.
		runner := &fakeRunner{
			res: infra.CmdResult{Stdout: []byte("partial-secret-output"), ExitCode: 1},
			err: errors.New("infra: user command: user-command ./hooks/get-token: exit 1"),
		}
		_, err := NewExecResolver("./hooks/get-token", runner).GetToken(context.Background(), "agent-api")
		errContains(t, err, "agent-api")
		if strings.Contains(err.Error(), "partial-secret-output") {
			t.Fatalf("error leaked stdout: %q", err.Error())
		}
	})
	t.Run("empty stdout", func(t *testing.T) {
		runner := &fakeRunner{res: infra.CmdResult{Stdout: []byte("\n")}}
		_, err := NewExecResolver("./hooks/get-token", runner).GetToken(context.Background(), "agent-api")
		errContains(t, err, "agent-api")
		errContains(t, err, "no token")
	})
	t.Run("no resolver configured", func(t *testing.T) {
		_, err := NewExecResolver("", &fakeRunner{}).GetToken(context.Background(), "agent-api")
		errContains(t, err, "no credentials.resolver")
	})
}
