//go:build !windows

package traceflow

import (
	"os"
	"path/filepath"
)

func replaceOutbox(temp, path string) error {
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		directory.Close()
		return err
	}
	return directory.Close()
}
