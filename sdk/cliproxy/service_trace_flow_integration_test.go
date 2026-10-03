package cliproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traceflow"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestTraceFlowServiceIntegration(t *testing.T) {
	// Service shutdown seals the global usage dispatcher; isolate this full lifecycle in a subprocess.
	if os.Getenv("TRACE_FLOW_TEST_SERVICE_CHILD") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestTraceFlowServiceIntegration$", "-test.v")
		command.Env = append(os.Environ(), "TRACE_FLOW_TEST_SERVICE_CHILD=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("service integration: %v\n%s", err, output)
		}
		return
	}
	t.Setenv("TRACE_FLOW_INTEGRATION_API_KEY", "synthetic-intake-key")
	intakeSeen := make(chan *collector.ExportTraceServiceRequest, 2)
	intake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var export collector.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &export); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if r.URL.Path != "/v1/traces" || r.Header.Get("X-Trace-Flow-Api-Key") != "synthetic-intake-key" {
			t.Error("incorrect intake route or credential")
		}
		if bytes.Contains(body, []byte("synthetic-upstream-key")) || bytes.Contains(body, []byte("PRIVATE_PROMPT_CANARY")) {
			t.Error("private input reached intake")
		}
		intakeSeen <- &export
		w.Header().Set("X-Trace-Flow-Contract", "cliproxyapi.execution/2")
		w.Header().Set("X-Trace-Flow-Recording", "true")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"partialSuccess":{}}`)
	}))
	defer intake.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("upstream path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5\",\"service_tier\":\"standard\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5\",\"service_tier\":\"standard\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3,\"total_tokens\":8}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cfg := &config.Config{Host: "127.0.0.1", Port: port, AuthDir: filepath.Join(root, "auths"),
		TraceFlow: config.TraceFlowConfig{Enabled: true, Endpoint: intake.URL + "/v1/traces", OutboxPath: filepath.Join(root, "state/outbox.db"), APIKeyEnv: "TRACE_FLOW_INTEGRATION_API_KEY", MinFreeBytes: 1},
	}
	cfg.APIKeys = []string{"synthetic-client-key"}
	cfg.OpenAICompatibility = []config.OpenAICompatibility{{Name: "Compatible Provider", BaseURL: upstream.URL,
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "synthetic-upstream-key"}},
		Models:        []config.OpenAICompatibilityModel{{Name: "fixture-model", Alias: "fixture-model"}},
	}}
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: "+strconv.Itoa(port)+"\nauth-dir: "+cfg.AuthDir+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.UsageStatisticsEnabled = true
	cfg.RemoteManagement.SecretKey = "synthetic-management-key"
	ready := make(chan struct{})
	service, err := NewBuilder().WithConfig(cfg).WithConfigPath(configPath).WithHooks(Hooks{OnAfterStart: func(*Service) { close(ready) }}).Build()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	defer cancel()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("startup failed: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("startup did not finish")
	}
	request, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port), strings.NewReader(`{"model":"fixture-model","stream":true,"messages":[{"role":"user","content":"PRIVATE_PROMPT_CANARY"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer synthetic-client-key")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if errClose := response.Body.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	if err != nil || response.StatusCode != 200 || !bytes.Contains(body, []byte("ok")) {
		t.Fatalf("model response failed: status %d, error %v, body %s", response.StatusCode, err, body)
	}
	var export *collector.ExportTraceServiceRequest
	select {
	case export = <-intakeSeen:
	case <-time.After(20 * time.Second):
		t.Fatal("actual exporter did not deliver")
	}
	attrs := export.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes
	values := map[string]string{}
	for _, attr := range attrs {
		values[attr.Key] = attr.Value.GetStringValue()
		if attr.Value.GetIntValue() != 0 {
			values[attr.Key] = strconv.FormatInt(attr.Value.GetIntValue(), 10)
		}
	}
	if values["gen_ai.system"] != "openai-compatibility" || values["gen_ai.usage.total_tokens"] != "8" || values["cliproxyapi.account.coverage"] != "credential" {
		t.Fatalf("incorrect imported facts: %v", values)
	}
	if values["gen_ai.response.model"] != "gpt-5" || values["cliproxyapi.response.service_tier"] != "standard" {
		t.Fatalf("reported metadata missing: %v", values)
	}
	gui := redisqueue.PopOldest(10)
	if len(gui) != 1 {
		t.Fatalf("GUI feed got %d executions", len(gui))
	}
	var guiRecord map[string]any
	if err := json.Unmarshal(gui[0], &guiRecord); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(gui[0], []byte("AccountIdentity")) {
		t.Fatal("private account snapshot reached GUI feed")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	maintenance, err := traceflow.OpenMaintenance(traceflow.Config{OutboxPath: cfg.TraceFlow.OutboxPath})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := maintenance.Close(); err != nil {
			t.Error(err)
		}
	}()
	status, err := maintenance.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.States["pending"].Count != 0 || status.States["quarantined"].Count != 0 {
		t.Fatalf("accepted execution retained as pending: %+v", status)
	}
}
