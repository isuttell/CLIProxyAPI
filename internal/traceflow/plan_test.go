package traceflow

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usageidentity"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

var planInstallation = uuid.MustParse("11111111-1111-4111-8111-111111111111")

func codexSubscription(plan string) usage.AccountIdentity {
	return usageidentity.NewSelected("CodexExecutor", "codex", "credential.json", "", "", "", "").WithSubscriptionPlan(plan)
}

func claudeSubscription(plan string) usage.AccountIdentity {
	return usageidentity.NewSelected("ClaudeExecutor", "claude", "claude.json", "", "", "org", "account").WithSubscriptionPlan(plan)
}

func spanPlan(t *testing.T, record usage.Record, secret []byte) (string, bool) {
	t.Helper()
	item, _, reason, err := mapRecord(record, planInstallation, secret)
	if err != nil {
		t.Fatalf("map: %v %s", err, reason)
	}
	for _, attr := range item.ScopeSpans[0].Spans[0].Attributes {
		if attr.Key == "cliproxyapi.account.plan" {
			return attr.Value.GetStringValue(), true
		}
	}
	return "", false
}

func TestAccountPlanEnumMapping(t *testing.T) {
	for _, test := range []struct {
		family, raw, want string
	}{
		{"codex", "free", "chatgpt_free"},
		{"codex", "plus", "chatgpt_plus"},
		{"codex", " Pro ", "chatgpt_pro"},
		{"codex", "team", "chatgpt_team"},
		{"codex", "business", "chatgpt_team"},
		{"codex", "enterprise", "chatgpt_enterprise"},
		{"codex", "edu", "chatgpt_enterprise"},
		{"codex", "go", "unknown"},
		{"codex", "", "unknown"},
		{"claude", "pro", "claude_pro"},
		{"claude", "max_5x", "claude_max_5x"},
		{"claude", "max_20x", "claude_max_20x"},
		{"claude", "plus", "unknown"},
		{"claude", "", "unknown"},
	} {
		identity := codexSubscription(test.raw)
		if test.family == "claude" {
			identity = claudeSubscription(test.raw)
		}
		if got := accountPlan(identity, test.family); got != test.want {
			t.Errorf("accountPlan(%s, %q) = %q, want %q", test.family, test.raw, got, test.want)
		}
	}
	if got := accountPlan(codexSubscription("pro"), "claude"); got != "" {
		t.Fatalf("family mismatch plan = %q, want omitted", got)
	}
	gemini := usageidentity.NewSelected("GeminiExecutor", "gemini", "gemini.json", "", "", "", "").WithSubscriptionPlan("pro")
	if got := accountPlan(gemini, "gemini"); got != "" {
		t.Fatalf("family without plan enum = %q, want omitted", got)
	}
}

func TestMapRecordAccountPlanCoverageRules(t *testing.T) {
	secret := []byte("synthetic-secret")
	record := sampleRecord()
	record.AccountIdentity = codexSubscription("pro")
	if plan, ok := spanPlan(t, record, secret); !ok || plan != "chatgpt_pro" {
		t.Fatalf("credential coverage plan = %q %v, want chatgpt_pro", plan, ok)
	}
	record.AccountIdentity = codexSubscription("")
	if plan, ok := spanPlan(t, record, secret); !ok || plan != "unknown" {
		t.Fatalf("unreadable subscription plan = %q %v, want unknown", plan, ok)
	}
	claude := sampleRecord()
	claude.Provider, claude.ExecutorType, claude.Model = "claude", "ClaudeExecutor", "claude-opus-4"
	claude.AccountIdentity = claudeSubscription("max_20x")
	if plan, ok := spanPlan(t, claude, secret); !ok || plan != "claude_max_20x" {
		t.Fatalf("provider-account plan = %q %v, want claude_max_20x", plan, ok)
	}
	record.AccountIdentity = usageidentity.NewSelected("CodexExecutor", "codex", "api-key-entry", "", "", "", "")
	if plan, ok := spanPlan(t, record, secret); ok {
		t.Fatalf("API key credential sent plan %q", plan)
	}
	record.AccountIdentity = codexSubscription("pro")
	if plan, ok := spanPlan(t, record, nil); ok {
		t.Fatalf("unknown coverage sent plan %q", plan)
	}
	record.AccountIdentity = usage.AccountIdentity{}
	if plan, ok := spanPlan(t, record, secret); ok {
		t.Fatalf("missing snapshot sent plan %q", plan)
	}
}

func TestPlanFitsFullyPopulatedCodexSpan(t *testing.T) {
	record := sampleRecord()
	record.AccountIdentity = codexSubscription("plus")
	record.TraceID, record.Alias, record.ResponseModel = "abcd1234", "gpt-5-fast", "gpt-5-2026"
	record.ServiceTier, record.ResponseServiceTier = "auto", "default"
	record.SessionID, record.ParentSessionID = "canonical-child", "canonical-parent"
	record.Failed, record.Fail.StatusCode = true, 429
	record.InboundTraceID, record.InboundSpanID = "0123456789abcdef0123456789abcdef", "0123456789abcdef"
	record.NativeSource, record.NativeSessionID = "codex", "child"
	record.NativeParentSessionID, record.NativeOriginSessionID = "root", "origin"
	item, _, reason, err := mapRecord(record, planInstallation, []byte("synthetic-secret"))
	if err != nil {
		t.Fatalf("33-attribute span rejected: %v %s", err, reason)
	}
	if got := len(item.ScopeSpans[0].Spans[0].Attributes); got != 33 {
		t.Fatalf("fully populated Codex span has %d attributes, want 33", got)
	}
}

func TestAttributeCountCap(t *testing.T) {
	attrs := make([]*commonpb.KeyValue, 33)
	if err := checkAttributeCount(attrs); err != nil {
		t.Fatalf("33 attributes rejected: %v", err)
	}
	if err := checkAttributeCount(append(attrs, stringAttr("extra", "x"))); err == nil {
		t.Fatal("34 attributes accepted")
	}
}

// planSource marshals the way HandleUsage does, so the digest is the stored source hash.
func planSource(t *testing.T, box *outbox, plan string) sourceRecord {
	t.Helper()
	record := sampleRecord()
	record.AccountIdentity = codexSubscription(plan)
	item, id, reason, err := mapRecord(record, box.installation, box.secret)
	if err != nil {
		t.Fatalf("map: %v %s", err, reason)
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	return sourceRecord{ID: id, Schema: 2, Payload: payload, Digest: sourceDigest(payload), Binding: "binding"}
}

func TestPlanSnapshotKeepsRetriesByteIdentical(t *testing.T) {
	box := testOutbox(t)
	source := planSource(t, box, "pro")
	if again := planSource(t, box, "pro"); again.Digest != source.Digest {
		t.Fatal("same plan snapshot produced a different source hash")
	}
	batch := commitSource(t, box, source)
	// The credential's plan changes after the execution was captured; the stored source must not follow it.
	upgraded := planSource(t, box, "plus")
	if upgraded.Digest == source.Digest {
		t.Fatal("plan is not part of the source hash")
	}
	if _, dup, conflict, _, err := box.commit([]sourceRecord{upgraded}); err != nil || dup != 0 || conflict != 1 {
		t.Fatalf("changed plan for a captured execution: dup=%d conflict=%d err=%v", dup, conflict, err)
	}
	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		bodies = append(bodies, raw)
		first := len(bodies) == 1
		mu.Unlock()
		if first {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("X-Trace-Flow-Contract", contractMarker)
		w.Header().Set("X-Trace-Flow-Recording", "true")
		_, _ = w.Write([]byte(`{"partialSuccess":{}}`))
	}))
	defer server.Close()
	e := testExporter(box, server)
	e.enabled.Store(true)
	if _, err := e.deliver(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	retry, err := box.batch("binding")
	if err != nil || len(retry) != 1 {
		t.Fatalf("retry batch: %d %v", len(retry), err)
	}
	if _, err := e.deliver(context.Background(), retry); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("retry changed the request body across %d deliveries", len(bodies))
	}
	if !bytes.Contains(bodies[1], []byte("chatgpt_pro")) || bytes.Contains(bodies[1], []byte("chatgpt_plus")) {
		t.Fatal("retry did not carry the plan captured at execution time")
	}
}
