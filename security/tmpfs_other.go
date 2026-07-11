//go:build !linux

package security

import "errors"

// isTmpfsDir fails closed on platforms without a tmpfs check: file-mode
// credentials require a verifiable RAM-backed scratch, and guessing would
// risk writing a raw token to disk.
func isTmpfsDir(string) (bool, error) {
	return false, errors.New("tmpfs verification is unsupported on this platform; file-mode credentials require a Linux tmpfs scratch dir")
}
