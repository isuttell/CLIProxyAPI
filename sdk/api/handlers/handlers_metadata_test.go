package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"golang.org/x/net/context"
)

func TestGetContextWithCancelCapturesClientRequestMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginCtx.Request.RemoteAddr = "192.0.2.10:43123"
	ginCtx.Request.Header.Add("X-Forwarded-For", "203.0.113.5")
	ginCtx.Request.Header.Add("X-Forwarded-For", "198.51.100.8")
	ginCtx.Request.Header.Set("User-Agent", "test-client/1.0")

	handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	ctx, cancel := handler.GetContextWithCancel(nil, ginCtx, context.Background())
	defer cancel()

	metadata := logging.GetClientRequestMetadata(ctx)
	if metadata.ClientIP != "192.0.2.10" {
		t.Fatalf("ClientIP = %q, want direct peer IP", metadata.ClientIP)
	}
	if metadata.XForwardedFor != "203.0.113.5, 198.51.100.8" {
		t.Fatalf("XForwardedFor = %q", metadata.XForwardedFor)
	}
	if metadata.UserAgent != "test-client/1.0" {
		t.Fatalf("UserAgent = %q", metadata.UserAgent)
	}
}

func TestGetContextWithCancelCapturesResolvedClientIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, engine := gin.CreateTestContext(httptest.NewRecorder())
	if errSetTrustedProxies := engine.SetTrustedProxies([]string{"192.0.2.0/24"}); errSetTrustedProxies != nil {
		t.Fatalf("SetTrustedProxies: %v", errSetTrustedProxies)
	}
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginCtx.Request.RemoteAddr = "192.0.2.10:43123"
	ginCtx.Request.Header.Set("X-Forwarded-For", "203.0.113.5")

	handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	ctx, cancel := handler.GetContextWithCancel(nil, ginCtx, context.Background())
	defer cancel()

	metadata := logging.GetClientRequestMetadata(ctx)
	if metadata.ResolvedClientIP != "203.0.113.5" {
		t.Fatalf("ResolvedClientIP = %q, want %q", metadata.ResolvedClientIP, "203.0.113.5")
	}
	if metadata.ClientIP != "192.0.2.10" {
		t.Fatalf("ClientIP = %q, want direct peer IP", metadata.ClientIP)
	}
}

func TestRequestExecutionMetadataIncludesExecutionSessionWithoutIdempotencyKey(t *testing.T) {
	ctx := WithExecutionSessionID(context.Background(), "session-1")

	meta := requestExecutionMetadata(ctx)
	if got := meta[coreexecutor.ExecutionSessionMetadataKey]; got != "session-1" {
		t.Fatalf("ExecutionSessionMetadataKey = %v, want %q", got, "session-1")
	}
	if _, ok := meta[idempotencyKeyMetadataKey]; ok {
		t.Fatalf("unexpected idempotency key in metadata: %v", meta[idempotencyKeyMetadataKey])
	}
}

func TestRequestExecutionMetadataIncludesHashedCallerScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginCtx.Set("userApiKey", "downstream-secret")
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	meta := requestExecutionMetadata(ctx)
	got, _ := meta[coreexecutor.CallerScopeMetadataKey].(string)
	want := coresession.CallerScope("downstream-secret")
	if got != want {
		t.Fatalf("CallerScopeMetadataKey = %q, want %q", got, want)
	}
	if got == "downstream-secret" {
		t.Fatal("caller scope contains the raw downstream credential")
	}
}

func TestRequestExecutionMetadataTraceCallbackWebsocketDetection(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("skips websocket upgrade", func(t *testing.T) {
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
		ginCtx.Request.Header.Set("Connection", "Upgrade")
		ginCtx.Request.Header.Set("Upgrade", "websocket")
		logging.SetGinRequestID(ginCtx, "1234abcd")
		ctx := context.WithValue(context.Background(), "gin", ginCtx)

		meta := requestExecutionMetadata(ctx)

		if _, exists := meta[coreexecutor.SelectedAuthIndexCallbackMetadataKey]; exists {
			t.Fatal("unexpected selected auth index callback for websocket upgrade")
		}
	})

	t.Run("keeps callback for incomplete upgrade headers", func(t *testing.T) {
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		ginCtx.Request.Header.Set("Upgrade", "websocket")
		logging.SetGinRequestID(ginCtx, "1234abcd")
		ctx := context.WithValue(context.Background(), "gin", ginCtx)

		meta := requestExecutionMetadata(ctx)

		if _, exists := meta[coreexecutor.SelectedAuthIndexCallbackMetadataKey]; !exists {
			t.Fatal("missing selected auth index callback for ordinary HTTP request")
		}
	})
}

func TestSetReasoningEffortMetadataUsesSuffixOverBody(t *testing.T) {
	meta := make(map[string]any)

	setReasoningEffortMetadata(meta, "openai", "gpt-5.4(high)", []byte(`{"reasoning_effort":"low"}`))

	if got := meta[coreexecutor.ReasoningEffortMetadataKey]; got != "high" {
		t.Fatalf("ReasoningEffortMetadataKey = %v, want %q", got, "high")
	}
}

func TestSetReasoningEffortMetadataSupportsOpenAIResponses(t *testing.T) {
	meta := make(map[string]any)

	setReasoningEffortMetadata(meta, "openai-response", "gpt-5.4", []byte(`{"reasoning":{"effort":"medium"}}`))

	if got := meta[coreexecutor.ReasoningEffortMetadataKey]; got != "medium" {
		t.Fatalf("ReasoningEffortMetadataKey = %v, want %q", got, "medium")
	}
}

func TestSetServiceTierMetadataExtractsValue(t *testing.T) {
	meta := make(map[string]any)

	setServiceTierMetadata(meta, []byte(`{"service_tier":"priority"}`))

	gotServiceTier := meta[coreexecutor.ServiceTierMetadataKey]
	if gotServiceTier != "priority" {
		t.Fatalf("ServiceTierMetadataKey = %v, want %q", gotServiceTier, "priority")
	}
}

func TestSetServiceTierMetadataDefaultsWhenMissing(t *testing.T) {
	meta := make(map[string]any)

	setServiceTierMetadata(meta, []byte(`{"model":"gpt-5.4"}`))

	gotServiceTier := meta[coreexecutor.ServiceTierMetadataKey]
	if gotServiceTier != "auto" {
		t.Fatalf("ServiceTierMetadataKey = %v, want %q", gotServiceTier, "auto")
	}
}

func TestSetServiceTierMetadataPreservesExplicitDefault(t *testing.T) {
	meta := make(map[string]any)

	setServiceTierMetadata(meta, []byte(`{"service_tier":"default"}`))

	if gotServiceTier := meta[coreexecutor.ServiceTierMetadataKey]; gotServiceTier != "default" {
		t.Fatalf("ServiceTierMetadataKey = %v, want %q", gotServiceTier, "default")
	}
}

func TestSetGenerateMetadataDefaultsWhenMissing(t *testing.T) {
	meta := make(map[string]any)

	setGenerateMetadata(meta, []byte(`{"model":"gpt-5.4"}`))

	if got := meta[coreexecutor.GenerateMetadataKey]; got != true {
		t.Fatalf("GenerateMetadataKey = %v, want true", got)
	}
}

func TestSetGenerateMetadataPreservesTrue(t *testing.T) {
	meta := make(map[string]any)

	setGenerateMetadata(meta, []byte(`{"generate":true}`))

	if got := meta[coreexecutor.GenerateMetadataKey]; got != true {
		t.Fatalf("GenerateMetadataKey = %v, want true", got)
	}
}

func TestSetGenerateMetadataHonorsExplicitFalse(t *testing.T) {
	meta := make(map[string]any)

	setGenerateMetadata(meta, []byte(`{"generate":false}`))

	if got := meta[coreexecutor.GenerateMetadataKey]; got != false {
		t.Fatalf("GenerateMetadataKey = %v, want false", got)
	}
}

func TestExtractSessionIDsFromRequestCanonicalHierarchy(t *testing.T) {
	// 1. Claude Code main session
	reqMain := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	reqMain.Header.Set("X-Claude-Code-Session-Id", "sess-claude-main")
	sid, pid := extractSessionIDsFromRequest(reqMain)
	if sid != "claude:sess-claude-main" || pid != "" {
		t.Fatalf("claude main session = (%q, %q), want (claude:sess-claude-main, empty)", sid, pid)
	}

	// 2. Claude Code subagent with parent agent
	reqSub := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	reqSub.Header.Set("X-Claude-Code-Session-Id", "sess-claude-main")
	reqSub.Header.Set("X-Claude-Code-Agent-Id", "agent-worker-1")
	reqSub.Header.Set("X-Claude-Code-Parent-Agent-Id", "agent-orchestrator")
	sid, pid = extractSessionIDsFromRequest(reqSub)
	if sid != "claude:sess-claude-main:agent:agent-worker-1" || pid != "claude:sess-claude-main:agent:agent-orchestrator" {
		t.Fatalf("claude subagent = (%q, %q), want (claude:sess-claude-main:agent:agent-worker-1, claude:sess-claude-main:agent:agent-orchestrator)", sid, pid)
	}

	// 3. Generic X-Session-ID and X-Parent-Session-ID
	reqGeneric := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqGeneric.Header.Set("X-Session-ID", "sess-worker-99")
	reqGeneric.Header.Set("X-Parent-Session-ID", "sess-root-1")
	sid, pid = extractSessionIDsFromRequest(reqGeneric)
	if sid != "header:sess-worker-99" || pid != "header:sess-root-1" {
		t.Fatalf("generic header session = (%q, %q), want (header:sess-worker-99, header:sess-root-1)", sid, pid)
	}

	// 4. Codex Session-Id
	reqCodex := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	reqCodex.Header.Set("Session-Id", "codex-sess-uuid")
	sid, pid = extractSessionIDsFromRequest(reqCodex)
	if sid != "codex:codex-sess-uuid" || pid != "" {
		t.Fatalf("codex session = (%q, %q), want (codex:codex-sess-uuid, empty)", sid, pid)
	}
}

func TestEnrichContextWithSessionHierarchyFromBody(t *testing.T) {
	ctx := context.Background()

	// 1. Body payload contains session_id and parent_session_id
	bodyJSON := []byte(`{"session_id":"body-task-1","parent_session_id":"body-parent-task"}`)
	ctx = enrichContextWithSessionHierarchy(ctx, nil, bodyJSON, nil)
	meta := logging.GetClientRequestMetadata(ctx)
	if meta.SessionID != "session:body-task-1" || meta.ParentSessionID != "session:body-parent-task" {
		t.Fatalf("body session = (%q, %q), want (session:body-task-1, session:body-parent-task)", meta.SessionID, meta.ParentSessionID)
	}

	// 2. Claude Code header combined with metadata.agent_id in body
	headers := http.Header{"X-Claude-Code-Session-Id": []string{"claude-root-001"}}
	subagentBody := []byte(`{"metadata":{"agent_id":"search-specialist"}}`)
	ctx2 := enrichContextWithSessionHierarchy(context.Background(), headers, subagentBody, nil)
	meta2 := logging.GetClientRequestMetadata(ctx2)
	if meta2.SessionID != "claude:claude-root-001:agent:search-specialist" || meta2.ParentSessionID != "claude:claude-root-001" {
		t.Fatalf("claude body subagent = (%q, %q), want (claude:claude-root-001:agent:search-specialist, claude:claude-root-001)", meta2.SessionID, meta2.ParentSessionID)
	}

	// 3. Ghost Parent avoidance: Context had parent, but body resolves to a top-level root session
	ctxWithOldParent := logging.WithClientRequestMetadata(context.Background(), logging.ClientRequestMetadata{
		SessionID:       "header:old-child",
		ParentSessionID: "header:old-parent",
	})
	topLevelBody := []byte(`{"session_id":"top-level-session"}`)
	ctx3 := enrichContextWithSessionHierarchy(ctxWithOldParent, nil, topLevelBody, nil)
	meta3 := logging.GetClientRequestMetadata(ctx3)
	if meta3.SessionID != "session:top-level-session" || meta3.ParentSessionID != "" {
		t.Fatalf("top level body session = (%q, %q), want (session:top-level-session, empty parent)", meta3.SessionID, meta3.ParentSessionID)
	}

	// 4. Self-loop avoidance: SessionID == ParentSessionID
	selfLoopBody := []byte(`{"session_id":"same-id","parent_session_id":"same-id"}`)
	ctx4 := enrichContextWithSessionHierarchy(context.Background(), nil, selfLoopBody, nil)
	meta4 := logging.GetClientRequestMetadata(ctx4)
	if meta4.SessionID != "session:same-id" || meta4.ParentSessionID != "" {
		t.Fatalf("self loop session = (%q, %q), want (session:same-id, empty parent)", meta4.SessionID, meta4.ParentSessionID)
	}

	// 5. Length bound: ID length is bounded to 256 bytes
	longID := strings.Repeat("a", 250)
	longBody := []byte(`{"session_id":"` + longID + `"}`)
	ctx5 := EnrichContextWithSessionHierarchy(context.Background(), nil, longBody, nil)
	meta5 := logging.GetClientRequestMetadata(ctx5)
	if len(meta5.SessionID) > 256 {
		t.Fatalf("SessionID length = %d, want <= 256", len(meta5.SessionID))
	}
	if !strings.HasPrefix(meta5.SessionID, "session:") {
		t.Fatalf("SessionID = %q, want session: prefix", meta5.SessionID)
	}

	// 6. Clearing hierarchy when interceptor removes all session headers and body has no session
	ctxWithHeaderSession := logging.WithClientRequestMetadata(context.Background(), logging.ClientRequestMetadata{
		SessionID:       "header:cleared-session",
		ParentSessionID: "header:cleared-parent",
	})
	ctxCleared := EnrichContextWithSessionHierarchy(ctxWithHeaderSession, nil, nil, nil)
	metaCleared := logging.GetClientRequestMetadata(ctxCleared)
	if metaCleared.SessionID != "" || metaCleared.ParentSessionID != "" {
		t.Fatalf("cleared context = (%q, %q), want (empty, empty)", metaCleared.SessionID, metaCleared.ParentSessionID)
	}

	// 7. Roo Code task delegation from body
	rooBody := []byte(`{"taskId":"task-child-99","parentTaskId":"task-parent-99"}`)
	ctx7 := EnrichContextWithSessionHierarchy(context.Background(), nil, rooBody, nil)
	meta7 := logging.GetClientRequestMetadata(ctx7)
	if meta7.SessionID != "task:task-child-99" || meta7.ParentSessionID != "task:task-parent-99" {
		t.Fatalf("Roo Code task session = (%q, %q), want (task:task-child-99, task:task-parent-99)", meta7.SessionID, meta7.ParentSessionID)
	}

	// 8. OpenCode parent_id in payload
	opencodeBody := []byte(`{"session_id":"opencode-sess-1","parent_id":"opencode-root-1"}`)
	ctx8 := EnrichContextWithSessionHierarchy(context.Background(), nil, opencodeBody, nil)
	meta8 := logging.GetClientRequestMetadata(ctx8)
	if meta8.SessionID != "session:opencode-sess-1" || meta8.ParentSessionID != "session:opencode-root-1" {
		t.Fatalf("OpenCode parent_id session = (%q, %q), want (session:opencode-sess-1, session:opencode-root-1)", meta8.SessionID, meta8.ParentSessionID)
	}
}

func TestEnrichContextCapturesNativeMetadataBeforeRoutingRewrite(t *testing.T) {
	headers := http.Header{
		"Thread-Id": {"child-thread"}, "Session-Id": {"origin-session"},
		"X-Codex-Turn-Metadata": {`{"parent_thread_id":"root-thread"}`},
		"Traceparent":           {"00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"},
	}
	ctx := EnrichContextWithSessionHierarchy(context.Background(), headers, nil, nil)
	ctx = EnrichContextWithSessionHierarchy(ctx, http.Header{"Session-Id": {"rewritten-session"}}, nil, nil)
	meta := logging.GetClientRequestMetadata(ctx)
	if meta.NativeSource != "codex" || meta.NativeSessionID != "child-thread" || meta.NativeParentSessionID != "root-thread" || meta.NativeOriginSessionID != "origin-session" || meta.InboundTraceID != "0123456789abcdef0123456789abcdef" || meta.InboundSpanID != "0123456789abcdef" {
		t.Fatalf("native metadata changed on re-enrichment: %+v", meta)
	}
}

func TestEnrichContextOmitsInboundTraceparentOnWebsocketTurn(t *testing.T) {
	ctx := coreexecutor.WithDownstreamWebsocket(context.Background())
	headers := http.Header{"Originator": {"codex_cli"}, "Thread-Id": {"child-thread"}, "Traceparent": {"00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"}}
	meta := logging.GetClientRequestMetadata(EnrichContextWithSessionHierarchy(ctx, headers, nil, nil))
	if meta.NativeSessionID != "child-thread" || meta.InboundTraceID != "" || meta.InboundSpanID != "" || meta.InboundTraceparentInvalid {
		t.Fatalf("websocket turn metadata: %+v", meta)
	}
}

func TestSharedNativeMetadataFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../internal/traceflow/testdata/cliproxyapi-session-metadata-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name     string      `json:"name"`
			Headers  http.Header `json:"headers"`
			Body     string      `json:"body"`
			Expected struct {
				Source             string `json:"source"`
				SessionID          string `json:"session_id"`
				AgentID            string `json:"agent_id"`
				ParentSessionID    string `json:"parent_session_id"`
				OriginSessionID    string `json:"origin_session_id"`
				InboundTraceID     string `json:"inbound_trace_id"`
				InboundSpanID      string `json:"inbound_span_id"`
				SessionIDAmbiguous bool   `json:"session_id_ambiguous"`
			} `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			ctx := EnrichContextWithSessionHierarchy(context.Background(), test.Headers, []byte(test.Body), nil)
			got := logging.GetClientRequestMetadata(ctx)
			want := test.Expected
			if got.NativeSource != want.Source || got.NativeSessionID != want.SessionID || got.NativeAgentID != want.AgentID || got.NativeParentSessionID != want.ParentSessionID || got.NativeOriginSessionID != want.OriginSessionID || got.InboundTraceID != want.InboundTraceID || got.InboundSpanID != want.InboundSpanID || got.NativeSessionIDAmbiguous != want.SessionIDAmbiguous {
				t.Fatalf("shared fixture mismatch: %+v", got)
			}
		})
	}
}
