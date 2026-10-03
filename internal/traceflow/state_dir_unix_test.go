//go:build !windows

package traceflow

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExistingStateDirectoryPermissionsArePreserved(t *testing.T) {
	parent := t.TempDir()
	shared := filepath.Join(parent, "shared")
	if err := os.Mkdir(shared, 0755); err != nil {
		t.Fatal(err)
	}
	_, err := openOutbox(filepath.Join(shared, "outbox.db"), 1<<20, 1)
	if err == nil {
		t.Fatal("accepted public state directory")
	}
	info, err := os.Stat(shared)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("changed existing directory permissions: %o", info.Mode().Perm())
	}
}
