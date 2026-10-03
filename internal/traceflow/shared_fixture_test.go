package traceflow

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

type sharedValue struct {
	StringValue string `json:"stringValue"`
	IntValue    string `json:"intValue"`
	BoolValue   *bool  `json:"boolValue"`
}
type sharedAttribute struct {
	Key   string      `json:"key"`
	Value sharedValue `json:"value"`
}
type sharedSpan struct {
	TraceID string `json:"traceId"`
	SpanID  string `json:"spanId"`
	Name    string `json:"name"`
	Kind    int    `json:"kind"`
	Start   string `json:"startTimeUnixNano"`
	End     string `json:"endTimeUnixNano"`
	Status  struct {
		Code int `json:"code"`
	} `json:"status"`
	Attributes []sharedAttribute `json:"attributes"`
}

func (s sharedSpan) attrs() map[string]sharedValue {
	result := make(map[string]sharedValue, len(s.Attributes))
	for _, attribute := range s.Attributes {
		result[attribute.Key] = attribute.Value
	}
	return result
}
func sharedInt(t *testing.T, attrs map[string]sharedValue, key string) int64 {
	t.Helper()
	value := attrs[key].IntValue
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
func TestSharedExecutionFixtureSemantics(t *testing.T) {
	raw, err := os.ReadFile("testdata/execution-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Scope struct{ Name, Version string }
				Spans []sharedSpan `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	installation := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	count := 0
	for _, resource := range fixture.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			if scope.Scope.Name != "cliproxyapi.execution" || scope.Scope.Version != "2" {
				t.Fatal("shared scope changed")
			}
			for _, source := range scope.Spans {
				count++
				attrs := source.attrs()
				start, err := strconv.ParseInt(source.Start, 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				end, err := strconv.ParseInt(source.End, 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				present := attrs["gen_ai.usage.missing"].BoolValue == nil
				record := usage.Record{RequestID: attrs["cliproxyapi.execution.id"].StringValue, Provider: "codex", ExecutorType: "CodexExecutor", Model: source.Name, RequestedAt: time.Unix(0, start), Latency: time.Duration(end - start), UsagePresent: &present, Failed: source.Status.Code == 2, TraceID: attrs["cliproxyapi.request.id"].StringValue, Alias: attrs["cliproxyapi.request.model_alias"].StringValue, ResponseModel: attrs["gen_ai.response.model"].StringValue, ServiceTier: attrs["cliproxyapi.request.service_tier"].StringValue, ResponseServiceTier: attrs["cliproxyapi.response.service_tier"].StringValue, SessionID: attrs["cliproxyapi.session.id"].StringValue, ParentSessionID: attrs["cliproxyapi.session.parent_id"].StringValue}
				if stream := attrs["gen_ai.streaming"].BoolValue; stream != nil {
					record.Stream = *stream
				}
				if status, ok := attrs["http.response.status_code"]; ok {
					record.Fail.StatusCode = int(sharedInt(t, map[string]sharedValue{"status": status}, "status"))
				}
				if ttft, ok := attrs["gen_ai.server.time_to_first_token"]; ok {
					record.TTFTPresent = true
					record.TTFT = time.Duration(sharedInt(t, map[string]sharedValue{"ttft": ttft}, "ttft")) * time.Millisecond
				}
				if present {
					record.Detail.TokenBreakdown = usage.TokenBreakdown{SchemaVersion: int(sharedInt(t, attrs, "gen_ai.usage.schema_version")), Quality: usage.TokenAccountingQuality(attrs["gen_ai.usage.quality"].StringValue), TotalTokens: sharedInt(t, attrs, "gen_ai.usage.total_tokens"), Input: usage.TokenInputBreakdown{TotalTokens: sharedInt(t, attrs, "gen_ai.usage.input_tokens"), UncachedTokens: sharedInt(t, attrs, "gen_ai.usage.input_tokens_uncached"), CacheReadTokens: sharedInt(t, attrs, "gen_ai.usage.cache_read_input_tokens"), CacheWriteTokens: sharedInt(t, attrs, "gen_ai.usage.cache_creation_input_tokens")}, Output: usage.TokenOutputBreakdown{TotalTokens: sharedInt(t, attrs, "gen_ai.usage.output_tokens"), NonReasoningTokens: sharedInt(t, attrs, "gen_ai.usage.output_tokens_non_reasoning"), ReasoningTokens: sharedInt(t, attrs, "gen_ai.usage.reasoning_tokens")}, UnclassifiedTokens: sharedInt(t, attrs, "gen_ai.usage.unclassified_tokens")}
				}
				item, _, reason, err := mapRecord(record, installation, []byte("synthetic-fixture-secret"))
				if err != nil {
					t.Fatalf("fixture span %d: %v %s", count, err, reason)
				}
				mapped := item.ScopeSpans[0].Spans[0]
				if hex.EncodeToString(mapped.TraceId) != source.TraceID || hex.EncodeToString(mapped.SpanId) != source.SpanID || mapped.Name != source.Name || int(mapped.Kind) != source.Kind || mapped.StartTimeUnixNano != uint64(start) || mapped.EndTimeUnixNano != uint64(end) || int(mapped.Status.Code) != source.Status.Code {
					t.Fatalf("fixture identity/timing/status mismatch on span %d", count)
				}
				actual := map[string]sharedValue{}
				for _, attr := range mapped.Attributes {
					value := sharedValue{}
					switch attr.Value.Value.(type) {
					case *commonpb.AnyValue_StringValue:
						value.StringValue = attr.Value.GetStringValue()
					case *commonpb.AnyValue_IntValue:
						value.IntValue = strconv.FormatInt(attr.Value.GetIntValue(), 10)
					case *commonpb.AnyValue_BoolValue:
						b := attr.Value.GetBoolValue()
						value.BoolValue = &b
					}
					actual[attr.Key] = value
				}
				for key, want := range attrs {
					if key == "gen_ai.system" || strings.HasPrefix(key, "cliproxyapi.account.") {
						continue
					}
					got, ok := actual[key]
					if !ok || got.StringValue != want.StringValue || got.IntValue != want.IntValue || (got.BoolValue == nil) != (want.BoolValue == nil) || got.BoolValue != nil && *got.BoolValue != *want.BoolValue {
						t.Errorf("fixture span %d attribute %s mismatch: got %+v want %+v", count, key, got, want)
					}
				}
			}
		}
	}
	if count != 5 {
		t.Fatalf("shared fixture had %d spans, expected 5", count)
	}
}
