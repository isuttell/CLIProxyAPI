package auth

import (
	"strconv"
	"testing"
	"time"
)

func TestClaudeQuotaWindowParsingRejectsMalformedAndIncomplete(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	for _, value := range []string{"NaN", "+Inf", "-0.1", "1.1"} {
		signals := map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": value,
			"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
		}
		windows, problems := claudeQuotaWindows(QuotaState{Signals: signals, ObservedAt: now})
		if len(windows) != 0 || len(problems) == 0 {
			t.Fatalf("utilization %q yielded windows=%v problems=%v", value, windows, problems)
		}
	}
	signals := map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.2",
		"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.3",
	}
	windows, problems := claudeQuotaWindows(QuotaState{Signals: signals, ObservedAt: now})
	if len(windows) != 1 || len(problems) == 0 || quotaStandingForSnapshot(windows, problems, now).fresh {
		t.Fatalf("partial snapshot yielded windows=%v problems=%v", windows, problems)
	}
	signals["Anthropic-Ratelimit-Unified-7d-Reset"] = "9223372036854775807"
	windows, problems = claudeQuotaWindows(QuotaState{Signals: signals, ObservedAt: now})
	if len(windows) != 1 || len(problems) == 0 {
		t.Fatalf("overflowing reset yielded windows=%v problems=%v", windows, problems)
	}
}

func TestCodexQuotaWindowParsingRejectsOverflowAndInvalidUtilization(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		used, minutes, reset string
	}{
		{"NaN", "300", "60"},
		{"101", "300", "60"},
		{"20", "9223372036854775807", "60"},
		{"20", "300", "9223372036854775807"},
		{"20", "0", "60"},
	} {
		signals := map[string]string{
			"X-Codex-Primary-Used-Percent":        tc.used,
			"X-Codex-Primary-Window-Minutes":      tc.minutes,
			"X-Codex-Primary-Reset-After-Seconds": tc.reset,
		}
		if _, present, valid := parseCodexWindow(QuotaState{Signals: signals, ObservedAt: now}, "X-Codex-Primary-"); !present || valid {
			t.Fatalf("invalid codex window accepted: %+v", tc)
		}
	}
}

func TestCodexAdditionalWindowMatchesRequestedModelOnly(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	reset := strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
	signals := map[string]string{
		"X-Codex-Primary-Used-Percent": "10", "X-Codex-Primary-Window-Minutes": "300", "X-Codex-Primary-Reset-At": reset,
		"X-Codex-Secondary-Used-Percent": "20", "X-Codex-Secondary-Window-Minutes": "10080", "X-Codex-Secondary-Reset-At": reset,
		"X-Codex-Additional-Gpt-5.3-Codex-Spark-Limit-Name":             "GPT-5.3-Codex-Spark",
		"X-Codex-Additional-Gpt-5.3-Codex-Spark-Primary-Used-Percent":   "99",
		"X-Codex-Additional-Gpt-5.3-Codex-Spark-Primary-Window-Minutes": "300",
		"X-Codex-Additional-Gpt-5.3-Codex-Spark-Primary-Reset-At":       reset,
		"X-Codex-Additional-Other-Limit-Name":                           "other-model",
		"X-Codex-Additional-Other-Primary-Used-Percent":                 "100",
		"X-Codex-Additional-Other-Primary-Window-Minutes":               "300",
		"X-Codex-Additional-Other-Primary-Reset-At":                     reset,
	}
	windows, problems := codexQuotaWindows(QuotaState{Signals: signals, ObservedAt: now}, "gpt-5.3-codex-spark")
	if len(problems) != 0 || len(windows) != 3 || quotaStandingFor(windows, now).exhausted || quotaStandingFor(windows, now).headroom > .011 {
		t.Fatalf("matching model windows=%v problems=%v", windows, problems)
	}
	windows, problems = codexQuotaWindows(QuotaState{Signals: signals, ObservedAt: now}, "unrelated-model")
	if len(problems) != 0 || len(windows) != 2 || quotaStandingFor(windows, now).exhausted {
		t.Fatalf("unrelated model windows=%v problems=%v", windows, problems)
	}
	prefix := "X-Codex-Additional-Gpt-5.3-Codex-Spark-Primary-"
	for _, suffix := range []string{"Used-Percent", "Window-Minutes", "Reset-At"} {
		delete(signals, prefix+suffix)
	}
	windows, problems = codexQuotaWindows(QuotaState{Signals: signals, ObservedAt: now}, "gpt-5.3-codex-spark")
	standing := quotaStandingForSnapshot(windows, problems, now)
	if len(problems) == 0 || standing.fresh {
		t.Fatalf("missing model measurements standing=%+v problems=%v", standing, problems)
	}
}

func TestClaudeQuotaWindowsUseCarriedSignalObservationTime(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	carriedAt := now.Add(-2 * time.Hour)
	reset := strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
	quota := QuotaState{
		ObservedAt: now,
		Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": "0.2",
			"Anthropic-Ratelimit-Unified-5h-Reset":       reset,
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.3",
			"Anthropic-Ratelimit-Unified-7d-Reset":       reset,
		},
		SignalObservedAt: map[string]time.Time{"Anthropic-Ratelimit-Unified-7d-Utilization": carriedAt},
	}
	windows, problems := claudeQuotaWindows(quota)
	if len(windows) != 2 || len(problems) != 0 {
		t.Fatalf("windows=%v problems=%v", windows, problems)
	}
	if !windows[0].observedAt.Equal(now) || !windows[1].observedAt.Equal(carriedAt) {
		t.Fatalf("window observation times = %v, %v", windows[0].observedAt, windows[1].observedAt)
	}
	if quotaStandingFor(windows, now).fresh {
		t.Fatal("snapshot with a stale carried window was reported fresh")
	}
}
