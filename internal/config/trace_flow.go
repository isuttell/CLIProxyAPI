package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TraceFlowConfig names the local outbox and the environment variable holding its intake key.
type TraceFlowConfig struct {
	Enabled         bool   `yaml:"enabled" json:"enabled"`
	Endpoint        string `yaml:"endpoint" json:"endpoint"`
	OutboxPath      string `yaml:"outbox-path" json:"outbox-path"`
	APIKeyEnv       string `yaml:"api-key-env" json:"api-key-env"`
	MaxPendingBytes int64  `yaml:"max-pending-bytes" json:"max-pending-bytes"`
	MinFreeBytes    int64  `yaml:"min-free-bytes" json:"min-free-bytes"`
}

// ResolvePaths keeps private exporter state outside credential storage, including symlink aliases.
func (c TraceFlowConfig) ResolvePaths(configPath, authDir string) (TraceFlowConfig, error) {
	if c.OutboxPath == "" {
		return c, fmt.Errorf("trace flow: outbox-path is required")
	}
	base, errBase := filepath.Abs(filepath.Dir(configPath))
	if errBase != nil {
		return c, fmt.Errorf("trace flow: resolve config directory: %w", errBase)
	}
	outbox, errOutbox := privateStatePath(c.OutboxPath, base)
	if errOutbox != nil {
		return c, errOutbox
	}
	if authDir != "" {
		auth, errAuth := privateStatePath(authDir, base)
		if errAuth != nil {
			return c, errAuth
		}
		rel, errRel := filepath.Rel(auth, outbox)
		if errRel != nil {
			return c, fmt.Errorf("trace flow: compare outbox and credential storage: %w", errRel)
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return c, fmt.Errorf("trace flow: outbox-path must be outside auth-dir")
		}
	}
	c.OutboxPath = outbox
	return c, nil
}

func privateStatePath(value, base string) (string, error) {
	if value == "~" || strings.HasPrefix(value, "~/") {
		homeDir, errHome := os.UserHomeDir()
		if errHome != nil {
			return "", fmt.Errorf("trace flow: resolve home directory: %w", errHome)
		}
		if value == "~" {
			value = homeDir
		} else {
			value = filepath.Join(homeDir, strings.TrimPrefix(value, "~/"))
		}
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, value)
	}
	value = filepath.Clean(value)
	ancestor := value
	var tail []string
	for {
		resolved, errResolve := filepath.EvalSymlinks(ancestor)
		if errResolve == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(errResolve) {
			return "", fmt.Errorf("trace flow: resolve state path: %w", errResolve)
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", fmt.Errorf("trace flow: state path has no existing ancestor")
		}
		tail = append(tail, filepath.Base(ancestor))
		ancestor = parent
	}
}
