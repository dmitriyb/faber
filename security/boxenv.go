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
	// the forwarded per-step agent socket.
	EnvSSHAuthSock = "SSH_AUTH_SOCK"

	// ContainerAgentSocket is where the identity binding mounts the step's
	// ephemeral agent socket inside the box.
	ContainerAgentSocket = "/ssh-agent"

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
