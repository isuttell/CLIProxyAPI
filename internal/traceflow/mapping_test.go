package traceflow

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usageidentity"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"google.golang.org/protobuf/proto"
)

func sampleRecord() usage.Record {
	present := true
	return usage.Record{RequestID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1", Provider: "codex", ExecutorType: "CodexExecutor", Model: "gpt-5", RequestedAt: time.Unix(1760000000, 0), Latency: time.Second, TTFT: 120 * time.Millisecond, TTFTPresent: true, UsagePresent: &present, AccountIdentity: usageidentity.NewSelected("CodexExecutor", "codex", "credential.json", "", "", "", ""), Detail: usage.Detail{TokenBreakdown: usage.NewSubsetTokenBreakdown(13, 2, 1, 10, 3, 23)}}
}
func TestMapRecordContractAndWhitelist(t *testing.T) {
	record := sampleRecord()
	canary := "sk-proj-123456789012345678901234567890"
	record.APIKey = canary
	record.BaseURL = "https://" + canary
	record.AuthID = canary
	record.Fail.Body = canary
	record.ResponseHeaders = map[string][]string{"Authorization": {canary}}
	item, id, reason, err := mapRecord(record, uuid.MustParse("11111111-1111-4111-8111-111111111111"), []byte("synthetic-installation-secret"))
	if err != nil {
		t.Fatalf("map: %v %s", err, reason)
	}
	if id != record.RequestID {
		t.Fatal("execution identity changed")
	}
	if len(item.ScopeSpans) != 1 || len(item.ScopeSpans[0].Spans) != 1 || item.ScopeSpans[0].Scope.Name != "cliproxyapi.execution" || item.ScopeSpans[0].Scope.Version != "2" {
		t.Fatal("wrong scope")
	}
	span := item.ScopeSpans[0].Spans[0]
	if len(span.Attributes) > 32 || span.StartTimeUnixNano != uint64(record.RequestedAt.UnixNano()) || span.EndTimeUnixNano != uint64(record.RequestedAt.Add(time.Second).UnixNano()) {
		t.Fatal("invalid span shape")
	}
	if len(span.TraceId) != 16 || len(span.SpanId) != 8 || !bytes.Equal(span.SpanId, span.TraceId[8:]) {
		t.Fatal("invalid OTel identity")
	}
	payload, err := proto.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte(canary)) {
		t.Fatal("secret escaped whitelist")
	}
	got := map[string]any{}
	for _, attr := range span.Attributes {
		got[attr.Key] = attr.Value.GetValue()
	}
	for _, required := range []string{"cliproxyapi.execution.id", "gen_ai.system", "gen_ai.request.model", "cliproxyapi.account.coverage", "cliproxyapi.account.ref", "gen_ai.usage.total_tokens", "gen_ai.server.time_to_first_token"} {
		if _, ok := got[required]; !ok {
			t.Errorf("missing %s", required)
		}
	}
}
func TestMapRecordRejectsBadEvidence(t *testing.T) {
	tests := []struct {
		name   string
		edit   func(*usage.Record)
		reason string
	}{
		{"noncanonical UUID", func(r *usage.Record) { r.RequestID = strings.ToUpper(r.RequestID) }, "execution_id_invalid"},
		{"wrong UUID version", func(r *usage.Record) { r.RequestID = "aaaaaaaa-aaaa-9aaa-8aaa-aaaaaaaaaaa1" }, "execution_id_invalid"},
		{"missing family", func(r *usage.Record) { r.ExecutorType = "PluginExecutor" }, "provider_family_unknown"},
		{"invalid model", func(r *usage.Record) { r.Model = "sk-proj-123456789012345678901234567890" }, "model_invalid"},
		{"invalid usage", func(r *usage.Record) { r.Detail.TokenBreakdown.TotalTokens++ }, "usage_invalid"},
		{"negative latency", func(r *usage.Record) { r.Latency = -time.Second }, "timing_invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := sampleRecord()
			test.edit(&r)
			_, _, reason, err := mapRecord(r, uuid.New(), []byte("synthetic-secret"))
			if err == nil || reason != test.reason {
				t.Fatalf("got %v %q", err, reason)
			}
		})
	}
}
func TestMapRecordUsagePresenceAndTTFT(t *testing.T) {
	r := sampleRecord()
	r.UsagePresent = nil
	r.Detail = usage.Detail{TokenBreakdown: usage.NewUnclassifiedTokenBreakdown(0)}
	r.TTFTPresent = false
	item, _, _, err := mapRecord(r, uuid.New(), []byte("synthetic-secret"))
	if err != nil {
		t.Fatal(err)
	}
	attrs := item.ScopeSpans[0].Spans[0].Attributes
	seen := map[string]bool{}
	for _, a := range attrs {
		seen[a.Key] = true
	}
	if !seen["gen_ai.usage.missing"] || seen["gen_ai.usage.total_tokens"] || seen["gen_ai.server.time_to_first_token"] {
		t.Fatal("missing usage or TTFT misreported")
	}
	present := true
	r.UsagePresent = &present
	r.TTFTPresent = true
	r.TTFT = 1500 * time.Millisecond
	item, _, _, err = mapRecord(r, uuid.New(), []byte("synthetic-secret"))
	if err != nil {
		t.Fatal(err)
	}
	seen = map[string]bool{}
	for _, a := range item.ScopeSpans[0].Spans[0].Attributes {
		seen[a.Key] = true
	}
	if !seen["gen_ai.usage.total_tokens"] || seen["gen_ai.usage.missing"] || seen["gen_ai.server.time_to_first_token"] {
		t.Fatal("known zero or out-of-range TTFT misreported")
	}
}

func TestOmissionDiagnosticsPersist(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Trace-Flow-Contract", contractMarker)
		w.Header().Set("X-Trace-Flow-Recording", "true")
		_, _ = w.Write([]byte(`{"partialSuccess":{}}`))
	}))
	defer server.Close()
	t.Setenv("TF_TEST_OMISSION_KEY", "synthetic-omission-key")
	cfg := Config{Enabled: true, Endpoint: server.URL + "/v1/traces", OutboxPath: filepath.Join(t.TempDir(), "private-state", "outbox.db"), APIKeyEnv: "TF_TEST_OMISSION_KEY", MaxPendingBytes: 1 << 20, MinFreeBytes: 1}
	exporter, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	record := sampleRecord()
	record.Stream = true
	record.TTFTPresent = false
	record.Alias = "alias@example.com"
	record.ResponseModel = ""
	record.ResponseServiceTier = ""
	exporter.HandleUsage(context.Background(), record)
	if err := exporter.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	maintenance, err := OpenMaintenance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	status, err := maintenance.Status()
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"alias_invalid", "response_model_missing", "response_tier_missing", "token_ttft_missing"} {
		if status.Omissions[reason] != 1 {
			t.Errorf("missing omission %s: %+v", reason, status.Omissions)
		}
	}
	if status.CaptureRejections != 0 {
		t.Fatalf("optional invalid value rejected capture: %+v", status)
	}
}
