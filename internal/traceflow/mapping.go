package traceflow

import (
	"encoding/hex"
	"errors"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usageidentity"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

var (
	uuidPattern     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	modelPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)
	token32Pattern  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,32}$`)
	token64Pattern  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
	token128Pattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	traceIDPattern  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanIDPattern   = regexp.MustCompile(`^[0-9a-f]{16}$`)
	emailPattern    = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+`)
	secretPattern   = regexp.MustCompile(`(?:sk-(?:proj-)?[A-Za-z0-9_-]{20,}|ghp_[A-Za-z0-9]{20,}|xox[baprs]-[A-Za-z0-9-]{20,}|eyJ[A-Za-z0-9_-]{30,})`)
)

func familyFor(record usage.Record) string {
	return usageidentity.Family(record.ExecutorType, record.Provider)
}

func stringAttr(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}
func intAttr(key string, value int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: value}}}
}
func boolAttr(key string, value bool) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: value}}}
}

func validExecutionID(id string) bool {
	if !uuidPattern.MatchString(id) {
		return false
	}
	parsed, err := uuid.Parse(id)
	return err == nil && parsed != uuid.Nil && parsed != uuid.Max
}

func mapRecord(record usage.Record, installation uuid.UUID, secret []byte) (*tracepb.ResourceSpans, string, string, error) {
	id := record.RequestID
	if !validExecutionID(id) {
		return nil, "", "execution_id_invalid", errors.New("invalid execution ID")
	}
	family := familyFor(record)
	if family == "" {
		return nil, "", "provider_family_unknown", errors.New("provider family unknown")
	}
	if !modelPattern.MatchString(record.Model) || sensitive(record.Model) {
		return nil, "", "model_invalid", errors.New("invalid requested model")
	}
	if record.RequestedAt.IsZero() || record.Latency < 0 || record.Latency > 24*time.Hour {
		return nil, "", "timing_invalid", errors.New("invalid execution timing")
	}
	start := record.RequestedAt.UnixNano()
	if start <= 0 || record.Latency > time.Duration(math.MaxInt64-start) {
		return nil, "", "timing_invalid", errors.New("invalid execution timing")
	}
	end := start + int64(record.Latency)
	traceID, _ := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	spanID := append([]byte(nil), traceID[8:]...)
	identity := record.AccountIdentity
	if !identity.IsCredentialSnapshot() {
		identity = usage.AccountIdentity{}
	}
	coverage, ref := accountReference(secret, identity, family)
	attrs := []*commonpb.KeyValue{stringAttr("cliproxyapi.execution.id", id), stringAttr("gen_ai.system", family), stringAttr("gen_ai.request.model", record.Model), stringAttr("cliproxyapi.account.coverage", coverage), boolAttr("gen_ai.streaming", record.Stream)}
	if ref != "" {
		attrs = append(attrs, stringAttr("cliproxyapi.account.ref", ref))
	}
	if token64Pattern.MatchString(record.TraceID) && !sensitive(record.TraceID) {
		attrs = append(attrs, stringAttr("cliproxyapi.request.id", record.TraceID))
	}
	if modelPattern.MatchString(record.Alias) && !sensitive(record.Alias) {
		attrs = append(attrs, stringAttr("cliproxyapi.request.model_alias", record.Alias))
	}
	if modelPattern.MatchString(record.ResponseModel) && !sensitive(record.ResponseModel) {
		attrs = append(attrs, stringAttr("gen_ai.response.model", record.ResponseModel))
	}
	if token32Pattern.MatchString(record.ServiceTier) && !sensitive(record.ServiceTier) {
		attrs = append(attrs, stringAttr("cliproxyapi.request.service_tier", record.ServiceTier))
	}
	if token32Pattern.MatchString(record.ResponseServiceTier) && !sensitive(record.ResponseServiceTier) {
		attrs = append(attrs, stringAttr("cliproxyapi.response.service_tier", record.ResponseServiceTier))
	}
	if token128Pattern.MatchString(record.SessionID) && !sensitive(record.SessionID) {
		attrs = append(attrs, stringAttr("cliproxyapi.session.id", record.SessionID))
	}
	if token128Pattern.MatchString(record.ParentSessionID) && !sensitive(record.ParentSessionID) {
		attrs = append(attrs, stringAttr("cliproxyapi.session.parent_id", record.ParentSessionID))
	}
	if validNativeSession(record) {
		attrs = append(attrs, stringAttr("cliproxyapi.client.source", record.NativeSource), stringAttr("cliproxyapi.client.session.id", record.NativeSessionID))
		if record.NativeSource == "claude" && validNativeID(record.NativeAgentID) && !strings.EqualFold(record.NativeAgentID, "main") {
			attrs = append(attrs, stringAttr("cliproxyapi.client.agent.id", record.NativeAgentID))
		}
		if record.NativeSource == "codex" {
			if validNativeID(record.NativeParentSessionID) && record.NativeParentSessionID != record.NativeSessionID {
				attrs = append(attrs, stringAttr("cliproxyapi.client.session.parent_id", record.NativeParentSessionID))
			}
			if validNativeID(record.NativeOriginSessionID) && record.NativeOriginSessionID != record.NativeSessionID {
				attrs = append(attrs, stringAttr("cliproxyapi.client.session.origin_id", record.NativeOriginSessionID))
			}
		}
	}
	if validInboundTrace(record) {
		attrs = append(attrs, stringAttr("cliproxyapi.inbound.trace_id", record.InboundTraceID), stringAttr("cliproxyapi.inbound.span_id", record.InboundSpanID))
	}
	if record.TTFTPresent && record.TTFT >= 0 && record.TTFT <= record.Latency {
		attrs = append(attrs, intAttr("gen_ai.server.time_to_first_token", int64(record.TTFT/time.Millisecond)))
	}
	if record.Failed && record.Fail.StatusCode >= 100 && record.Fail.StatusCode <= 599 {
		attrs = append(attrs, intAttr("http.response.status_code", int64(record.Fail.StatusCode)))
	}
	if record.HasUsage() {
		breakdown := record.Detail.TokenBreakdown
		if !breakdown.Valid() || breakdown.TotalTokens > math.MaxUint32 {
			return nil, "", "usage_invalid", errors.New("invalid token accounting")
		}
		attrs = append(attrs, intAttr("gen_ai.usage.schema_version", 2), stringAttr("gen_ai.usage.quality", string(breakdown.Quality)),
			intAttr("gen_ai.usage.total_tokens", breakdown.TotalTokens), intAttr("gen_ai.usage.input_tokens", breakdown.Input.TotalTokens),
			intAttr("gen_ai.usage.input_tokens_uncached", breakdown.Input.UncachedTokens), intAttr("gen_ai.usage.cache_read_input_tokens", breakdown.Input.CacheReadTokens),
			intAttr("gen_ai.usage.cache_creation_input_tokens", breakdown.Input.CacheWriteTokens), intAttr("gen_ai.usage.output_tokens", breakdown.Output.TotalTokens),
			intAttr("gen_ai.usage.output_tokens_non_reasoning", breakdown.Output.NonReasoningTokens), intAttr("gen_ai.usage.reasoning_tokens", breakdown.Output.ReasoningTokens),
			intAttr("gen_ai.usage.unclassified_tokens", breakdown.UnclassifiedTokens))
	} else {
		attrs = append(attrs, boolAttr("gen_ai.usage.missing", true))
	}
	if len(attrs) > 32 {
		return nil, "", "attribute_count", errors.New("too many span attributes")
	}
	code := tracepb.Status_STATUS_CODE_OK
	if record.Failed {
		code = tracepb.Status_STATUS_CODE_ERROR
	}
	span := &tracepb.Span{TraceId: traceID, SpanId: spanID, Name: record.Model, Kind: tracepb.Span_SPAN_KIND_SERVER, StartTimeUnixNano: uint64(start), EndTimeUnixNano: uint64(end), Status: &tracepb.Status{Code: code}, Attributes: attrs}
	result := &tracepb.ResourceSpans{Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{stringAttr("cliproxyapi.installation.id", installation.String()), stringAttr("service.name", "CLIProxyAPI")}}, ScopeSpans: []*tracepb.ScopeSpans{{Scope: &commonpb.InstrumentationScope{Name: "cliproxyapi.execution", Version: "2"}, Spans: []*tracepb.Span{span}}}}
	return result, id, "", nil
}

func validNativeID(value string) bool {
	return token128Pattern.MatchString(value) && !sensitive(value)
}

func validNativeSession(record usage.Record) bool {
	return !nativeSessionAmbiguous(record) && (record.NativeSource == "claude" || record.NativeSource == "codex") && validNativeID(record.NativeSessionID)
}

func nativeSessionAmbiguous(record usage.Record) bool {
	return record.NativeSessionIDAmbiguous || record.NativeSource == "codex" && record.NativeSessionID != "" && record.NativeSessionID == record.NativeParentSessionID
}

func validInboundTrace(record usage.Record) bool {
	return traceIDPattern.MatchString(record.InboundTraceID) && spanIDPattern.MatchString(record.InboundSpanID) &&
		record.InboundTraceID != strings.Repeat("0", 32) && record.InboundSpanID != strings.Repeat("0", 16)
}

func sensitive(value string) bool {
	return emailPattern.MatchString(value) || secretPattern.MatchString(value)
}

func omissionReasons(record usage.Record) []string {
	var reasons []string
	if nativeSessionAmbiguous(record) {
		reasons = append(reasons, "client_session_ambiguous")
	} else if record.NativeSessionIDInvalid || record.NativeSessionID != "" && !validNativeID(record.NativeSessionID) {
		reasons = append(reasons, "client_session_id_invalid")
	}
	if record.NativeSource != "" && record.NativeSource != "claude" && record.NativeSource != "codex" || record.NativeSource == "" && record.NativeSessionID != "" || record.NativeSource != "" && record.NativeSessionID == "" && !nativeSessionAmbiguous(record) {
		reasons = append(reasons, "client_source_invalid")
	}
	if record.NativeAgentIDInvalid || record.NativeAgentID != "" && (!validNativeID(record.NativeAgentID) || strings.EqualFold(record.NativeAgentID, "main") || record.NativeSource != "claude" || !validNativeSession(record)) {
		reasons = append(reasons, "client_agent_id_invalid")
	}
	if record.NativeParentSessionIDInvalid || record.NativeParentSessionID != "" && (!validNativeID(record.NativeParentSessionID) || record.NativeSource != "codex" || !validNativeSession(record) && !nativeSessionAmbiguous(record)) {
		reasons = append(reasons, "client_parent_session_id_invalid")
	}
	if record.NativeOriginSessionIDInvalid || record.NativeOriginSessionID != "" && (!validNativeID(record.NativeOriginSessionID) || record.NativeSource != "codex" || !validNativeSession(record)) {
		reasons = append(reasons, "client_origin_session_id_invalid")
	}
	if record.InboundTraceparentInvalid || record.InboundTraceID != "" || record.InboundSpanID != "" {
		if !validInboundTrace(record) {
			reasons = append(reasons, "inbound_traceparent_invalid")
		}
	}
	if record.TraceID != "" && (!token64Pattern.MatchString(record.TraceID) || sensitive(record.TraceID)) {
		reasons = append(reasons, "request_id_invalid")
	}
	if record.Alias != "" && (!modelPattern.MatchString(record.Alias) || sensitive(record.Alias)) {
		reasons = append(reasons, "alias_invalid")
	}
	if record.ResponseModel == "" {
		reasons = append(reasons, "response_model_missing")
	} else if !modelPattern.MatchString(record.ResponseModel) || sensitive(record.ResponseModel) {
		reasons = append(reasons, "response_model_invalid")
	}
	if record.ServiceTier != "" && (!token32Pattern.MatchString(record.ServiceTier) || sensitive(record.ServiceTier)) {
		reasons = append(reasons, "request_tier_invalid")
	}
	if record.ResponseServiceTier == "" {
		reasons = append(reasons, "response_tier_missing")
	} else if !token32Pattern.MatchString(record.ResponseServiceTier) || sensitive(record.ResponseServiceTier) {
		reasons = append(reasons, "response_tier_invalid")
	}
	if record.SessionID != "" && (!token128Pattern.MatchString(record.SessionID) || sensitive(record.SessionID)) {
		reasons = append(reasons, "session_id_invalid")
	}
	if record.ParentSessionID != "" && (!token128Pattern.MatchString(record.ParentSessionID) || sensitive(record.ParentSessionID)) {
		reasons = append(reasons, "parent_session_id_invalid")
	}
	if record.TTFTPresent {
		if record.TTFT < 0 || record.TTFT > record.Latency {
			reasons = append(reasons, "token_ttft_out_of_range")
		}
	} else if record.Stream {
		reasons = append(reasons, "token_ttft_missing")
	}
	return reasons
}
