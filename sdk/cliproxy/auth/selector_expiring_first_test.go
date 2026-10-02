package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func claudeQuotaAuth(t *testing.T, id string, utilization5h, utilization7d string, reset7d time.Time, status7d string) *Auth {
	t.Helper()
	headers := http.Header{}
	headers.Set("anthropic-ratelimit-unified-5h-utilization", utilization5h)
	headers.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10))
	headers.Set("anthropic-ratelimit-unified-7d-utilization", utilization7d)
	headers.Set("anthropic-ratelimit-unified-7d-reset", strconv.FormatInt(reset7d.Unix(), 10))
	headers.Set("anthropic-ratelimit-unified-7d-status", status7d)
	auth := &Auth{ID: id, Provider: "claude", Status: StatusActive}
	if !auth.Quota.ObserveResponseHeadersForProvider("claude", headers, time.Now()) {
		t.Fatalf("precondition: claude headers for %s were not observed", id)
	}
	return auth
}

func codexQuotaAuth(t *testing.T, id string, secondaryUsed string, secondaryResetAt time.Time) *Auth {
	t.Helper()
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "10")
	headers.Set("x-codex-primary-window-minutes", "300")
	headers.Set("x-codex-primary-reset-at", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	headers.Set("x-codex-secondary-used-percent", secondaryUsed)
	headers.Set("x-codex-secondary-window-minutes", "10080")
	headers.Set("x-codex-secondary-reset-at", strconv.FormatInt(secondaryResetAt.Unix(), 10))
	auth := &Auth{ID: id, Provider: "codex", Status: StatusActive}
	if !auth.Quota.ObserveResponseHeadersForProvider("codex", headers, time.Now()) {
		t.Fatalf("precondition: codex headers for %s were not observed", id)
	}
	return auth
}

func pickExpiringFirst(t *testing.T, provider string, auths ...*Auth) string {
	t.Helper()
	got, err := (&ExpiringFirstSelector{}).Pick(context.Background(), provider, "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	return got.ID
}

func TestExpiringFirstSelector_PrefersEarliestWeeklyReset(t *testing.T) {
	t.Parallel()
	now := time.Now()
	later := claudeQuotaAuth(t, "a-later", "0.10", "0.90", now.Add(6*24*time.Hour), "allowed")
	sooner := claudeQuotaAuth(t, "b-sooner", "0.10", "0.20", now.Add(12*time.Hour), "allowed")

	if got := pickExpiringFirst(t, "claude", later, sooner); got != "b-sooner" {
		t.Fatalf("Pick() = %q, want %q", got, "b-sooner")
	}
}

func TestExpiringFirstSelector_UnknownRanksAfterKnown(t *testing.T) {
	t.Parallel()
	fresh := &Auth{ID: "a-fresh", Provider: "claude", Status: StatusActive}
	running := claudeQuotaAuth(t, "b-running", "0.10", "0.40", time.Now().Add(3*24*time.Hour), "allowed")

	if got := pickExpiringFirst(t, "claude", fresh, running); got != "b-running" {
		t.Fatalf("Pick() = %q, want %q", got, "b-running")
	}
}

func TestExpiringFirstSelector_ExhaustedRanksLast(t *testing.T) {
	t.Parallel()
	now := time.Now()
	rejected := claudeQuotaAuth(t, "a-rejected", "0.10", "0.99", now.Add(time.Hour), "rejected")
	full5h := claudeQuotaAuth(t, "b-full-5h", "1.0", "0.50", now.Add(2*time.Hour), "allowed")
	fresh := &Auth{ID: "c-fresh", Provider: "claude", Status: StatusActive}

	if got := pickExpiringFirst(t, "claude", rejected, full5h, fresh); got != "c-fresh" {
		t.Fatalf("Pick() = %q, want %q", got, "c-fresh")
	}
}

func TestExpiringFirstSelector_ElapsedWindowIsUnknown(t *testing.T) {
	t.Parallel()
	now := time.Now()
	stale := &Auth{ID: "a-stale", Provider: "claude", Status: StatusActive, Quota: QuotaState{
		ObservedAt: now.Add(-8 * 24 * time.Hour),
		Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.95",
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(now.Add(-time.Hour).Unix(), 10),
		},
	}}
	running := claudeQuotaAuth(t, "b-running", "0.10", "0.10", now.Add(5*24*time.Hour), "allowed")

	if got := pickExpiringFirst(t, "claude", stale, running); got != "b-running" {
		t.Fatalf("Pick() = %q, want %q", got, "b-running")
	}
}

func TestExpiringFirstSelector_CodexRanksBySecondaryWindow(t *testing.T) {
	t.Parallel()
	now := time.Now()
	later := codexQuotaAuth(t, "a-later", "80", now.Add(5*24*time.Hour))
	sooner := codexQuotaAuth(t, "b-sooner", "5", now.Add(20*time.Hour))

	if got := pickExpiringFirst(t, "codex", later, sooner); got != "b-sooner" {
		t.Fatalf("Pick() = %q, want %q", got, "b-sooner")
	}
}

func TestExpiringFirstSelector_CodexRelativeResetAnchorsAtObservation(t *testing.T) {
	t.Parallel()
	now := time.Now()
	relative := &Auth{ID: "a-relative", Provider: "codex", Status: StatusActive, Quota: QuotaState{
		ObservedAt: now,
		Signals: map[string]string{
			"X-Codex-Primary-Used-Percent":          "10",
			"X-Codex-Primary-Window-Minutes":        "300",
			"X-Codex-Primary-Reset-At":              strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
			"X-Codex-Secondary-Used-Percent":        "20",
			"X-Codex-Secondary-Window-Minutes":      "10080",
			"X-Codex-Secondary-Reset-After-Seconds": strconv.Itoa(int((48 * time.Hour).Seconds())),
		},
	}}
	absolute := codexQuotaAuth(t, "b-absolute", "20", now.Add(4*24*time.Hour))

	if got := pickExpiringFirst(t, "codex", absolute, relative); got != "a-relative" {
		t.Fatalf("Pick() = %q, want %q", got, "a-relative")
	}
}

func TestExpiringFirstSelector_StaleAccountLimitFlagDoesNotBury(t *testing.T) {
	t.Parallel()
	now := time.Now()
	recovered := claudeQuotaAuth(t, "a-recovered", "1.0", "0.40", now.Add(24*time.Hour), "allowed")
	recovered.Quota.Signals["Anthropic-Ratelimit-Unified-Status"] = "rejected"
	recovered.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(now.Add(-time.Minute).Unix(), 10)
	later := claudeQuotaAuth(t, "b-later", "0.10", "0.10", now.Add(5*24*time.Hour), "allowed")

	if got := pickExpiringFirst(t, "claude", later, recovered); got != "b-later" {
		t.Fatalf("Pick() = %q, want %q", got, "b-later")
	}
}

func TestExpiringFirstSelector_SubscriptionPrecedesMeteredKey(t *testing.T) {
	t.Parallel()
	subscription := claudeQuotaAuth(t, "subscription", "0.9", "0.9", time.Now().Add(24*time.Hour), "allowed")
	subscription.Attributes = map[string]string{AttributeAuthKind: AuthKindOAuth}
	apiKey := &Auth{ID: "key", Provider: "claude", Status: StatusActive, Attributes: map[string]string{AttributeAuthKind: AuthKindAPIKey}}
	if got := pickExpiringFirst(t, "claude", apiKey, subscription); got != subscription.ID {
		t.Fatalf("Pick() = %q, want subscription", got)
	}
}

func TestExpiringFirstSelector_ExhaustedSubscriptionYieldsToKey(t *testing.T) {
	t.Parallel()
	subscription := claudeQuotaAuth(t, "subscription", "1", "0.5", time.Now().Add(24*time.Hour), "allowed")
	subscription.Attributes = map[string]string{AttributeAuthKind: AuthKindOAuth}
	apiKey := &Auth{ID: "key", Provider: "claude", Status: StatusActive, Attributes: map[string]string{AttributeAuthKind: AuthKindAPIKey}}
	apiKey.Quota = subscription.Quota.Clone()
	if got := pickExpiringFirst(t, "claude", subscription, apiKey); got != apiKey.ID {
		t.Fatalf("Pick() = %q, want key", got)
	}
	if _, err := (&ExpiringFirstSelector{}).Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, []*Auth{subscription}); err == nil {
		t.Fatal("Pick() dispatched a credibly exhausted subscription")
	}
}

func TestManagerUpdate_QuotaSnapshotFollowsAccountIdentity(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, nil, nil)
	observed := claudeQuotaAuth(t, "quota-identity-auth", "0.10", "0.30", time.Now().Add(24*time.Hour), "allowed")
	observed.Metadata = map[string]any{"email": "one@example.com", "access_token": "token-1"}
	if _, errRegister := manager.Register(ctx, observed); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	refreshed, errRefresh := manager.Update(ctx, &Auth{ID: observed.ID, Provider: "claude", Status: StatusActive,
		Metadata: map[string]any{"email": "one@example.com", "access_token": "token-2"}})
	if errRefresh != nil {
		t.Fatalf("Update(token refresh) error = %v", errRefresh)
	}
	if len(refreshed.Quota.Signals) == 0 {
		t.Fatal("token refresh dropped the quota snapshot, want it kept for the same account")
	}

	swapped, errSwap := manager.Update(ctx, &Auth{ID: observed.ID, Provider: "claude", Status: StatusActive,
		Metadata: map[string]any{"email": "two@example.com", "access_token": "token-3"}})
	if errSwap != nil {
		t.Fatalf("Update(account swap) error = %v", errSwap)
	}
	if len(swapped.Quota.Signals) != 0 {
		t.Fatalf("account swap kept quota snapshot %v, want it discarded", swapped.Quota.Signals)
	}
}

func TestExpiringFirstSelector_NoSignalsFallsBackToIDOrder(t *testing.T) {
	t.Parallel()
	auths := []*Auth{{ID: "b"}, {ID: "a"}, {ID: "c"}}
	if got := pickExpiringFirst(t, "gemini", auths...); got != "a" {
		t.Fatalf("Pick() = %q, want %q", got, "a")
	}
}

func TestManagerPickNext_ExpiringFirstRoutesThroughSelector(t *testing.T) {
	now := time.Now()
	later := claudeQuotaAuth(t, "expiring-first-later", "0.10", "0.10", now.Add(6*24*time.Hour), "allowed")
	sooner := claudeQuotaAuth(t, "expiring-first-sooner", "0.10", "0.10", now.Add(24*time.Hour), "allowed")

	manager := NewManager(nil, &ExpiringFirstSelector{}, nil)
	manager.RegisterExecutor(&refreshMockExecutor{id: "claude"})
	reg := registry.GetGlobalRegistry()
	for _, auth := range []*Auth{later, sooner} {
		reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "claude-expiring-first-model"}})
		authID := auth.ID
		t.Cleanup(func() { reg.UnregisterClient(authID) })
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	if manager.useSchedulerFastPath() {
		t.Fatal("useSchedulerFastPath() = true, want expiring-first to bypass the incremental scheduler")
	}
	picked, _, errPick := manager.pickNext(context.Background(), "claude", "claude-expiring-first-model", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if picked.ID != sooner.ID {
		t.Fatalf("pickNext() = %q, want %q", picked.ID, sooner.ID)
	}
}
