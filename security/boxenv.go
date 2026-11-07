package security

import "strings"

// The box environment contract: the FABER_* names and in-container paths the
// bindings emit and the agent module's entry program consumes. Defined once
// here, never restated per binding.
const (
	// EnvRemoteURL carries the box's only git remote: the gateway URL prefix
	// with the step's repo input spliced in.
	EnvRemoteURL = "FABER_REMOTE_URL"

	// EnvHostKey carries the gateway's pinned public host-key line (pinned
	// mode: the box installs it and connects with StrictHostKeyChecking=yes,
	// fail closed).
	EnvHostKey = "FABER_HOST_KEY"

	// EnvHostKeyTOFU marks the sandbox-only trust-on-first-use opt-in (the
	// box connects with accept-new). Mutually exclusive with EnvHostKey.
	EnvHostKeyTOFU = "FABER_HOST_KEY_TOFU"

	// EnvSSHAuthSock is the standard ssh-agent socket variable, pointed at
	// the box's own agent socket, which the relay serves.
	EnvSSHAuthSock = "SSH_AUTH_SOCK"

	// ContainerAgentDir is the tmpfs the identity binding mounts for the
	// box's agent socket; ContainerAgentSocket is the socket `faber-box
	// relay` listens on inside it. Both are under /faber, a reserved path.
	ContainerAgentDir    = "/faber/agent"
	ContainerAgentSocket = "/faber/agent/agent.sock"

	// ContainerEntry is the baked box binary the relay is exec'd from; the
	// agent module owns the path and restates it (it imports this package).
	ContainerEntry = "/faber/bin/faber-box"

	// ContainerSecretsDir is where file-mode credential mounts land; the
	// box's secrets phase exports each file under it into the agent process
	// env (in-box process only, by the agent module's contract).
	ContainerSecretsDir = "/run/secrets"
)

// ServiceURLEnv names the base-URL env var for a proxy-mode credential
// service: FABER_SERVICE_<NAME>_URL. The box points the tool's base URL at
// it; the user's auth-injecting proxy behind the endpoint holds the real
// credential.
func ServiceURLEnv(service string) string {
	return "FABER_SERVICE_" + envToken(service) + "_URL"
}

// HelperEnv names one forwarded field of a helper-mode credential service:
// FABER_HELPER_<NAME>_<FIELD>. The box hooks install the fields into the
// tool's credential-helper configuration.
func HelperEnv(service, field string) string {
	return "FABER_HELPER_" + envToken(service) + "_" + field
}

// envToken uppercases a service name for env-var embedding, mapping "-" to
// "_" (the one substitution the contract defines).
func envToken(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}
