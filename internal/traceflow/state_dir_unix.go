//go:build !windows

package traceflow

import (
	"errors"
	"fmt"
	"os"
)

func ensurePrivateStateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("create trace flow state directory: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect trace flow state directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("trace flow state path is not a directory")
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("trace flow state directory must be private (0700), found %o", info.Mode().Perm())
	}
	return nil
}

func checkExistingPrivateStateFile(string) error { return nil }
