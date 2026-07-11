package failure

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
)

// InputHash keys journal reuse: the hex SHA-256 over a canonical encoding of
// the step's resolved input values, its template identity, and its image tag.
// Same slot values fed to the same template running the same image ⇒ the same
// hash, stable across processes and runs; any of the three changing changes
// the key and therefore forces a re-run on resume.
//
// Canonicalization matches the config module's IR emission discipline: fixed
// envelope, object keys sorted at every depth, HTML escaping off, and numbers
// preserved as written (raw JSON fragments are re-read with json.Number so no
// floats are introduced by round-tripping).
func InputHash(inputs map[string]any, template, imageTag string) (string, error) {
	canon, err := canonicalJSON(inputs)
	if err != nil {
		return "", fmt.Errorf("failure: input hash: %w", err)
	}
	h := sha256.New()
	enc := json.NewEncoder(h)
	enc.SetEscapeHTML(false)
	err = enc.Encode(struct {
		Inputs   json.RawMessage `json:"inputs"`
		Template string          `json:"template"`
		Image    string          `json:"image"`
	}{canon, template, imageTag})
	if err != nil {
		return "", fmt.Errorf("failure: input hash: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// canonicalJSON renders v with object keys sorted at every depth and HTML
// escaping off, so identical values always produce identical bytes.
func canonicalJSON(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
		return nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeScalar(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	case json.RawMessage:
		// Re-read the fragment (numbers kept verbatim via json.Number) and
		// canonicalize it like any other value.
		dec := json.NewDecoder(bytes.NewReader(t))
		dec.UseNumber()
		var inner any
		if err := dec.Decode(&inner); err != nil {
			return fmt.Errorf("raw JSON fragment: %w", err)
		}
		// A fragment with trailing bytes must fail loudly, not hash as its
		// prefix — otherwise distinct (malformed) inputs alias one key.
		if _, err := dec.Token(); err != io.EOF {
			return fmt.Errorf("raw JSON fragment: trailing data after value")
		}
		return writeCanonical(buf, inner)
	case json.Number:
		buf.WriteString(t.String())
		return nil
	default:
		return writeScalar(buf, v)
	}
}

// writeScalar encodes a leaf value with HTML escaping off.
func writeScalar(buf *bytes.Buffer, v any) error {
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	buf.Truncate(buf.Len() - 1) // Encode appends '\n'
	return nil
}

// HashFile is the hex SHA-256 of a file's bytes — used for the journal
// header's config hash.
func HashFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failure: hash %s: %w", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
