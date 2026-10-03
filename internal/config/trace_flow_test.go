package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestTraceFlowPrivateStatePath(t *testing.T) {
	root := t.TempDir()
	auth := filepath.Join(root, "auths")
	if err := os.Mkdir(auth, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := TraceFlowConfig{OutboxPath: "state/trace-flow.db"}
	resolved, err := cfg.ResolvePaths(filepath.Join(root, "config.yaml"), auth)
	if err != nil {
		t.Fatal(err)
	}
	actualRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.OutboxPath != filepath.Join(actualRoot, "state/trace-flow.db") {
		t.Fatalf("unexpected state path: %s", resolved.OutboxPath)
	}
	cfg.OutboxPath = "auths/nested/outbox.db"
	if _, err := cfg.ResolvePaths(filepath.Join(root, "config.yaml"), auth); err == nil {
		t.Fatal("accepted state inside credentials")
	}
	link := filepath.Join(root, "state-link")
	if err := os.Symlink(auth, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	cfg.OutboxPath = filepath.Join(link, "nested/outbox.db")
	if _, err := cfg.ResolvePaths(filepath.Join(root, "config.yaml"), auth); err == nil {
		t.Fatal("accepted symlink into credentials")
	}
}

func TestTraceFlowConfigStaysLocal(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("trace-flow:\n  enabled: true\n  endpoint: https://dev.example/v1/traces\n  outbox-path: state/outbox.db\n  api-key-env: TRACE_FLOW_TEST_KEY\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.TraceFlow.Enabled || cfg.TraceFlow.APIKeyEnv != "TRACE_FLOW_TEST_KEY" {
		t.Fatal("Trace Flow configuration was not loaded")
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "TRACE_FLOW_TEST_KEY") || strings.Contains(string(data), "trace-flow") {
		t.Fatal("local exporter configuration reached public config JSON")
	}
}

func TestTraceFlowV8RoundTripAndLegacyMigration(t *testing.T) {
	block := "enabled: true\n    endpoint: https://dev.example/v1/traces\n    outbox-path: state/trace-flow.db\n    api-key-env: TRACE_FLOW_TEST_KEY\n    max-pending-bytes: 8388608\n    min-free-bytes: 1024\n"
	canonical := []byte("observability:\n  trace-flow:\n    " + block)
	if err := ValidateV8Config(canonical); err != nil {
		t.Fatalf("canonical v8 block invalid: %v", err)
	}
	cfg, err := ParseConfigBytes(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.TraceFlow.Enabled || cfg.TraceFlow.Endpoint != "https://dev.example/v1/traces" || cfg.TraceFlow.MaxPendingBytes != 8388608 {
		t.Fatalf("canonical v8 block lost runtime values: %+v", cfg.TraceFlow)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, canonical, 0600); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfigPreserveComments(path, cfg, true); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateV8Config(saved); err != nil {
		t.Fatalf("saved config no longer v8: %v", err)
	}
	if !strings.Contains(string(saved), "observability:") || !strings.Contains(string(saved), "  trace-flow:") {
		t.Fatal("save lost canonical nested exporter path")
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.TraceFlow != cfg.TraceFlow {
		t.Fatalf("roundtrip changed exporter config: before=%+v after=%+v", cfg.TraceFlow, reloaded.TraceFlow)
	}

	legacy := []byte("trace-flow:\n  " + strings.ReplaceAll(block, "\n    ", "\n  "))
	before, err := ParseConfigBytes(legacy)
	if err != nil {
		t.Fatal(err)
	}
	migrated, changed, err := NormalizeConfigLayout(legacy, true)
	if err != nil || !changed {
		t.Fatalf("legacy migration: changed=%v error=%v", changed, err)
	}
	if err := ValidateV8Config(migrated); err != nil {
		t.Fatalf("migrated exporter block invalid: %v", err)
	}
	if !strings.Contains(string(migrated), "observability:") || !strings.Contains(string(migrated), "  trace-flow:") {
		t.Fatalf("legacy exporter block not moved: %s", migrated)
	}
	after, err := ParseConfigBytes(migrated)
	if err != nil {
		t.Fatal(err)
	}
	if after.TraceFlow != before.TraceFlow || after.TraceFlow != cfg.TraceFlow {
		t.Fatalf("migration changed exporter config: before=%+v after=%+v", before.TraceFlow, after.TraceFlow)
	}
}
