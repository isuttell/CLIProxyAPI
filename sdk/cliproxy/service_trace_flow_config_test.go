package cliproxy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/traceflow"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func privateTraceFlowOutbox(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if errChmod := os.Chmod(root, 0700); errChmod != nil {
		t.Fatal(errChmod)
	}
	return filepath.Join(root, "outbox.db")
}

func traceFlowReloadConfig(outbox string) config.TraceFlowConfig {
	return config.TraceFlowConfig{
		Enabled: true, Endpoint: "http://localhost/v1/traces",
		OutboxPath: outbox, APIKeyEnv: "TRACE_FLOW_RELOAD_TEST_KEY",
		MaxPendingBytes: 8 << 20, MinFreeBytes: 1,
	}
}

func TestTraceFlowReloadCannotEnableAfterDisabledStartup(t *testing.T) {
	original := &config.Config{}
	service := &Service{cfg: original}
	next := &config.Config{TraceFlow: traceFlowReloadConfig(privateTraceFlowOutbox(t))}
	if service.applyConfigUpdateWithAuthSynthesis(context.Background(), next, false) {
		t.Fatal("reload enabled exporter after disabled startup")
	}
	if service.cfg != original || service.configSequence != 0 {
		t.Fatal("rejected reload changed active config")
	}
}

func TestTraceFlowReloadToggleAndRestartOnlyFields(t *testing.T) {
	t.Setenv("TRACE_FLOW_RELOAD_TEST_KEY", "synthetic-test-key")
	initial := traceFlowReloadConfig(privateTraceFlowOutbox(t))
	exporter, err := traceflow.Open(traceFlowRuntimeConfig(initial))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if errClose := exporter.Close(context.Background()); errClose != nil {
			t.Errorf("close exporter: %v", errClose)
		}
	})
	service := &Service{cfg: &config.Config{TraceFlow: initial}, traceFlow: exporter, traceFlowConfig: initial}

	disabled := initial
	disabled.Enabled = false
	disabledCfg := &config.Config{TraceFlow: disabled}
	if !service.applyConfigUpdateWithAuthSynthesis(context.Background(), disabledCfg, false) {
		t.Fatal("reload rejected disable toggle")
	}
	status, err := exporter.Status()
	if err != nil || status.Enabled {
		t.Fatalf("exporter stayed enabled after disable: enabled=%v error=%v", status.Enabled, err)
	}
	enabledCfg := &config.Config{TraceFlow: initial}
	if !service.applyConfigUpdateWithAuthSynthesis(context.Background(), enabledCfg, false) {
		t.Fatal("reload rejected re-enable toggle")
	}
	status, err = exporter.Status()
	if err != nil || !status.Enabled {
		t.Fatalf("exporter stayed disabled after re-enable: enabled=%v error=%v", status.Enabled, err)
	}

	for _, tc := range []struct {
		name string
		edit func(*config.TraceFlowConfig)
	}{
		{"endpoint", func(c *config.TraceFlowConfig) { c.Endpoint = "https://different.example/v1/traces" }},
		{"outbox", func(c *config.TraceFlowConfig) { c.OutboxPath = filepath.Join(t.TempDir(), "other.db") }},
		{"key variable", func(c *config.TraceFlowConfig) { c.APIKeyEnv = "OTHER_TEST_KEY" }},
		{"pending capacity", func(c *config.TraceFlowConfig) { c.MaxPendingBytes++ }},
		{"free space", func(c *config.TraceFlowConfig) { c.MinFreeBytes++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := initial
			tc.edit(&changed)
			candidate := &config.Config{TraceFlow: changed}
			before := service.cfg
			sequence := service.configSequence
			if service.applyConfigUpdateWithAuthSynthesis(context.Background(), candidate, false) {
				t.Fatal("accepted restart-only change")
			}
			if service.cfg != before || service.configSequence != sequence || service.traceFlowConfig != initial {
				t.Fatal("rejected change modified active config")
			}
			status, err := exporter.Status()
			if err != nil || !status.Enabled {
				t.Fatalf("rejected change altered exporter: enabled=%v error=%v", status.Enabled, err)
			}
		})
	}
}

func TestHomeOverlayPreservesLocalTraceFlow(t *testing.T) {
	t.Setenv("TRACE_FLOW_RELOAD_TEST_KEY", "synthetic-test-key")
	local := traceFlowReloadConfig(privateTraceFlowOutbox(t))
	exporter, errOpen := traceflow.Open(traceFlowRuntimeConfig(local))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	t.Cleanup(func() {
		if errClose := exporter.Close(context.Background()); errClose != nil {
			t.Errorf("close exporter: %v", errClose)
		}
	})
	base := &config.Config{TraceFlow: local}
	base.Home.Enabled = true
	service := &Service{cfg: base, traceFlow: exporter, traceFlowConfig: local}
	remote := &config.Config{}
	remote.Routing.Strategy = "fill-first"
	client, _ := newHomePluginTaskTestClient(t, nil, 0)
	work, errStage := service.stageHomeOverlayWithClient(context.Background(), remote, client)
	if errStage != nil {
		t.Fatal(errStage)
	}
	if work.config == nil || work.config.TraceFlow != local {
		t.Fatalf("Home overlay changed local Trace Flow config: %+v", work.config)
	}
	if commit := service.commitConfigUpdate(work.config); commit.cfg == nil {
		t.Fatal("Home overlay rejected a valid remote update")
	}
	if service.cfg.Routing.Strategy != "fill-first" {
		t.Fatalf("remote routing strategy was not applied: %q", service.cfg.Routing.Strategy)
	}
}

type blockedUsagePlugin struct {
	entered chan struct{}
	release chan struct{}
}

func (p *blockedUsagePlugin) HandleUsage(context.Context, usage.Record) {
	close(p.entered)
	<-p.release
}

func TestDisabledTraceFlowShutdownDoesNotWaitForUsagePlugin(t *testing.T) {
	if os.Getenv("TRACE_FLOW_DISABLED_DRAIN_CHILD") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestDisabledTraceFlowShutdownDoesNotWaitForUsagePlugin$")
		command.Env = append(os.Environ(), "TRACE_FLOW_DISABLED_DRAIN_CHILD=1")
		if output, errRun := command.CombinedOutput(); errRun != nil {
			t.Fatalf("disabled Trace Flow shutdown: %v\n%s", errRun, output)
		}
		return
	}
	plugin := &blockedUsagePlugin{entered: make(chan struct{}), release: make(chan struct{})}
	usage.RegisterPlugin(plugin)
	usage.PublishRecord(context.Background(), usage.Record{})
	<-plugin.entered
	finished := make(chan error, 1)
	go func() { finished <- (&Service{}).drainTraceFlowUsage(context.Background()) }()
	select {
	case errDrain := <-finished:
		if errDrain != nil {
			t.Error(errDrain)
		}
	case <-time.After(2 * time.Second):
		t.Error("disabled exporter shutdown waited for a blocked usage plugin")
	}
	close(plugin.release)
	if errDrain := usage.StopDefaultAndWait(context.Background()); errDrain != nil {
		t.Fatal(errDrain)
	}
}
