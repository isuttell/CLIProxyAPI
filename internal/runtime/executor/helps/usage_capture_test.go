package helps

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestParserUsageEvidenceBeforeNormalization(t *testing.T) {
	for _, tc := range []struct {
		name    string
		parse   func([]byte) usage.Detail
		missing string
		null    string
		zero    string
	}{
		{"openai", ParseOpenAIUsage, `{"usage":{}}`, `{"usage":{"input_tokens":null}}`, `{"usage":{"input_tokens":0}}`},
		{"claude", ParseClaudeUsage, `{"usage":{}}`, `{"usage":{"input_tokens":null}}`, `{"usage":{"input_tokens":0}}`},
		{"gemini", ParseGeminiUsage, `{"usageMetadata":{}}`, `{"usageMetadata":{"promptTokenCount":null}}`, `{"usageMetadata":{"promptTokenCount":0}}`},
		{"interactions", ParseInteractionsUsage, `{"usage":{}}`, `{"usage":{"input_tokens":null}}`, `{"usage":{"input_tokens":0}}`},
		{"antigravity", ParseAntigravityUsage, `{"response":{"usageMetadata":{}}}`, `{"response":{"usageMetadata":{"promptTokenCount":null}}}`, `{"response":{"usageMetadata":{"promptTokenCount":0}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			missing := tc.parse([]byte(tc.missing))
			if missing.UsagePresent {
				t.Fatalf("empty usage marked present: %+v", missing)
			}
			null := tc.parse([]byte(tc.null))
			if null.UsagePresent {
				t.Fatalf("null count marked present: %+v", null)
			}
			zero := tc.parse([]byte(tc.zero))
			if !zero.UsagePresent || usage.HasNonZeroUsage(zero) {
				t.Fatalf("explicit zero evidence lost: %+v", zero)
			}
		})
	}
}

func TestStreamKeepsPartialUsageAcrossZeroAndFailure(t *testing.T) {
	var buffer StreamUsageBuffer
	buffer.ObserveOpenAIStream([]byte(`data: {"usage":{"input_tokens":7}}`))
	buffer.ObserveOpenAIStream([]byte(`data: {"usage":{"input_tokens":0,"output_tokens":0}}`))
	detail, ok := buffer.Detail()
	if !ok || !detail.UsagePresent || detail.InputTokens != 7 {
		t.Fatalf("partial usage overwritten by zero placeholder: %+v, %v", detail, ok)
	}
	reporter := NewUsageReporter(context.Background(), "openai", "model", nil)
	record := reporter.buildRecord(detail, true)
	if !record.Failed || !record.HasUsage() || record.Detail.InputTokens != 7 {
		t.Fatalf("failed record lost partial usage: %+v", record)
	}
}

func TestTokenTTFTDistinctFromFirstPacket(t *testing.T) {
	reporter := NewUsageReporter(context.Background(), "codex", "model", nil)
	reporter.StartResponseTTFT()
	reporter.RecordFirstPacket()
	firstPacket := reporter.buildRecord(usage.Detail{}, false)
	if firstPacket.TTFTPresent {
		t.Fatal("first packet must not count as token TTFT")
	}
	byteReporter := NewUsageReporter(context.Background(), "codex", "model", nil)
	byteReporter.StartResponseTTFT()
	byteReporter.MarkFirstResponseByte()
	if byteReporter.buildRecord(usage.Detail{}, false).TTFTPresent {
		t.Fatal("first response byte must not count as token TTFT")
	}
	reporter.ObserveTokenEvent(true)
	token := reporter.buildRecord(usage.Detail{}, false)
	if !token.TTFTPresent {
		t.Fatal("substantive token must mark TTFT present")
	}
}

func TestClaudeReportedServiceTier(t *testing.T) {
	complete := ParseClaudeUsage([]byte(`{"type":"message","usage":{"input_tokens":1,"output_tokens":2,"service_tier":"priority"}}`))
	if complete.ResponseServiceTier != "priority" {
		t.Fatalf("Anthropic message usage tier = %q", complete.ResponseServiceTier)
	}
	start := []byte(`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":0,"service_tier":"flex"}}}`)
	stream, ok := ParseClaudeStreamUsage(start)
	if !ok || !stream.UsagePresent || stream.ResponseServiceTier != "flex" {
		t.Fatalf("Anthropic message_start usage = %+v, %v", stream, ok)
	}
	var buffer StreamUsageBuffer
	buffer.ObserveClaudeStream(start)
	buffer.ObserveClaudeStream([]byte(`data: {"type":"message_delta","usage":{"output_tokens":2,"service_tier":"priority"}}`))
	merged, ok := buffer.Detail()
	if !ok || merged.ResponseServiceTier != "priority" || merged.InputTokens != 0 || merged.OutputTokens != 2 {
		t.Fatalf("Anthropic stream tier/usage merge = %+v, %v", merged, ok)
	}
}

func TestGeminiStreamZeroNeedsTerminalEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, early, terminal string
		parse                 func([]byte) (usage.Detail, bool)
	}{
		{"gemini", `data: {"usageMetadata":{"promptTokenCount":0,"totalTokenCount":0}}`, `data: {"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":0,"totalTokenCount":0}}`, ParseGeminiStreamUsage},
		{"antigravity", `data: {"response":{"usageMetadata":{"promptTokenCount":0,"totalTokenCount":0}}}`, `data: {"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":0,"totalTokenCount":0}}}`, ParseAntigravityStreamUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buffer StreamUsageBuffer
			detail, ok := tc.parse([]byte(tc.early))
			buffer.Observe(detail, ok)
			if _, present := buffer.Detail(); present {
				t.Fatal("provisional zero usage survived failed stream")
			}
			terminal, ok := tc.parse([]byte(tc.terminal))
			if !ok || !terminal.UsagePresent || usage.HasNonZeroUsage(terminal) {
				t.Fatalf("terminal explicit zero = %+v, %v", terminal, ok)
			}
		})
	}
}
