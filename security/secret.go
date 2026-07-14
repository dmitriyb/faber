package security

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
)

// redacted is what every formatting and marshalling path of a Secret yields.
const redacted = "[redacted]"

// Secret is an opaque credential value. It can be encoded into the stdin
// secrets payload; it cannot be printed: %s, %v, %+v, %#v, error wrapping,
// JSON, text, and slog output all yield "[redacted]". The raw bytes are
// reachable only through the unexported accessor inside this package, and only
// the stdin-payload encoder uses it — redaction is a property of the type
// system, not of discipline.
type Secret struct {
	v []byte
}

// NewSecret wraps raw credential bytes in the opaque type, copying them so
// the caller's buffer cannot mutate the secret after the fact.
func NewSecret(b []byte) Secret {
	return Secret{v: bytes.Clone(b)}
}

// String implements fmt.Stringer; it redacts.
func (Secret) String() string { return redacted }

// GoString implements fmt.GoStringer (%#v); it redacts.
func (Secret) GoString() string { return redacted }

// Format implements fmt.Formatter, redacting every verb (fmt prefers
// Formatter over Stringer/GoStringer, so this covers nested struct printing
// too).
func (Secret) Format(f fmt.State, _ rune) { io.WriteString(f, redacted) }

// MarshalJSON redacts JSON encoding (journal-bound records included).
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText redacts text-based encoders (encoding/text consumers, yaml.v3).
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// LogValue redacts structured log output regardless of handler.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// reveal returns the raw bytes. Unexported by design: only
// encodeSecretsPayload (set.go) calls it, at the single moment of encoding the
// file-mode tokens into the stdin secrets payload.
func (s Secret) reveal() []byte { return s.v }
