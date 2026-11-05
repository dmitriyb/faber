package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// ErrHookNotFound is CheckHookFile's common case, named so a caller can
// show the path alone and spell out only the rarer reasons.
var ErrHookNotFound = errors.New("not found")

// CheckHookFile is the one predicate for a hook script's path on the
// machine that runs it: present, a regular file, executable, and starting
// with an interpreter line. The error names the failing property in the
// words the run preflight and the live view both show. Validate never
// calls it — file presence is a run-machine fact, not a config fact.
func CheckHookFile(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrHookNotFound
		}
		return err
	}
	if st.IsDir() {
		return errors.New("a directory, not a script")
	}
	if !st.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	if st.Mode().Perm()&0o111 == 0 {
		return errors.New("not executable")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 2)
	if _, err := io.ReadFull(f, head); err != nil || string(head) != "#!" {
		return fmt.Errorf("no #! interpreter line")
	}
	return nil
}
