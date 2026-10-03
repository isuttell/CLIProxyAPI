//go:build windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTraceFlowStateOnSeparateVolume(t *testing.T) {
	configDir := t.TempDir()
	authDir := filepath.Join(configDir, "auths")
	if err := os.Mkdir(authDir, 0700); err != nil {
		t.Fatal(err)
	}
	var stateDir string
	for _, root := range []string{`C:\`, `D:\`} {
		if strings.EqualFold(filepath.VolumeName(root), filepath.VolumeName(configDir)) {
			continue
		}
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			continue
		}
		var err error
		stateDir, err = os.MkdirTemp(root, "trace-flow-config-")
		if err != nil {
			t.Fatal(err)
		}
		break
	}
	if stateDir == "" {
		t.Skip("requires two mounted Windows volumes")
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(stateDir); err != nil {
			t.Error(err)
		}
	})
	cfg := TraceFlowConfig{OutboxPath: filepath.Join(stateDir, "outbox.db")}
	resolved, err := cfg.ResolvePaths(filepath.Join(configDir, "config.yaml"), authDir)
	if err != nil {
		t.Fatalf("separate-volume private state: %v", err)
	}
	if resolved.OutboxPath != cfg.OutboxPath {
		t.Fatal("changed cross-volume state location")
	}
}
