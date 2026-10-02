package auth

import (
	"context"
	"math"
	"strconv"
	"testing"
	"time"
)

func standingWindow(now time.Time, used float64, length, untilReset time.Duration) quotaWindow {
	return quotaWindow{utilization: used, length: length, resetAt: now.Add(untilReset), observedAt: now}
}

func TestQuotaStandingUsesMostRestrictiveLiveWindow(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	short := standingWindow(now, .9, 5*time.Hour, 2*time.Hour)
	weekly := standingWindow(now, .2, 7*24*time.Hour, 3*24*time.Hour)
	standing := quotaStandingFor([]quotaWindow{short, weekly}, now)
	if !standing.known || !standing.fresh || math.Abs(standing.headroom-.1) > 1e-9 || standing.exhausted {
		t.Fatalf("standing = %+v, want fresh 10%% headroom", standing)
	}
	short.utilization = .1
	weekly.utilization = .85
	standing = quotaStandingFor([]quotaWindow{short, weekly}, now)
	if math.Abs(standing.headroom-.15) > 1e-9 {
		t.Fatalf("headroom = %v, want weekly 15%%", standing.headroom)
	}
}

func TestQuotaStandingStaleAndElapsedNeverClaimFreshCapacity(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	stale := standingWindow(now, .8, 7*24*time.Hour, 3*24*time.Hour)
	stale.observedAt = now.Add(-31 * time.Minute)
	standing := quotaStandingFor([]quotaWindow{stale}, now)
	if standing.fresh || math.Abs(standing.headroom-.2) > 1e-9 || standing.routingScore() > .2 {
		t.Fatalf("stale standing = %+v, want restricted uncertain score", standing)
	}
	stale.utilization = 1
	if !quotaStandingFor([]quotaWindow{stale}, now).exhausted {
		t.Fatal("live reported exhaustion was forgotten before reset")
	}
	stale.resetAt = now.Add(-time.Second)
	standing = quotaStandingFor([]quotaWindow{stale}, now)
	if standing.known || standing.exhausted || standing.fresh || standing.routingScore() != uncertainRoutingScoreCap {
		t.Fatalf("elapsed standing = %+v, want unknown without exhaustion", standing)
	}
	if got := quotaStandingFor(nil, now); got.known || got.headroom != 0 || got.routingScore() != uncertainRoutingScoreCap {
		t.Fatalf("missing quota = %+v, want no claimed balance", got)
	}
}

func TestQuotaStandingWeeklyUrgencyAndSessionLoad(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	soon := quotaStandingFor([]quotaWindow{standingWindow(now, .2, 7*24*time.Hour, 24*time.Hour)}, now)
	later := quotaStandingFor([]quotaWindow{standingWindow(now, .2, 7*24*time.Hour, 6*24*time.Hour)}, now)
	if soon.urgency <= later.urgency || !quotaStandingLess(soon, later, "soon", "later") {
		t.Fatalf("urgency soon=%v later=%v", soon.urgency, later.urgency)
	}
	soon.active = 4
	if !quotaStandingLess(later, soon, "later", "soon") {
		t.Fatalf("assigned sessions did not lower score: soon=%v later=%v", soon.routingScore(), later.routingScore())
	}
	if assignedSessionsFrom(withAssignedSessions(context.Background(), map[string]int{"a": 2}))["a"] != 2 {
		t.Fatal("assigned sessions were not passed through context")
	}
}

func TestQuotaStandingTiersAndChildGate(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	good := quotaStandingFor([]quotaWindow{standingWindow(now, .4, 5*time.Hour, time.Hour)}, now)
	good.kind = AuthKindOAuth
	unknown := quotaStandingFor(nil, now)
	unknown.kind = AuthKindOAuth
	low := quotaStandingFor([]quotaWindow{standingWindow(now, .9, 5*time.Hour, time.Hour)}, now)
	low.kind = AuthKindOAuth
	key := quotaStanding{kind: AuthKindAPIKey}
	exhausted := quotaStandingFor([]quotaWindow{standingWindow(now, 1, 5*time.Hour, time.Hour)}, now)
	exhausted.kind = AuthKindOAuth
	ordered := []quotaStanding{good, unknown, low, key, exhausted}
	for i := 0; i < len(ordered)-1; i++ {
		if !quotaStandingLess(ordered[i], ordered[i+1], "a", "b") {
			t.Fatalf("tier %d should precede tier %d", ordered[i].tier(), ordered[i+1].tier())
		}
	}
	selector := &ExpiringFirstSelector{}
	auth := &Auth{Provider: "claude", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.55",
		"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.1",
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(now.Add(24*time.Hour).Unix(), 10),
	}}}
	if selector.canInherit(auth, "model", now, 2) {
		t.Fatal("child was allowed below reserved 20% floor")
	}
	auth.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Utilization"] = "0.5"
	if !selector.canInherit(auth, "model", now, 2) {
		t.Fatal("child was denied at reserved 20% floor")
	}
	if selector.canInherit(&Auth{}, "model", now, 0) {
		t.Fatal("child inherited unknown quota")
	}
}

func TestQuotaStandingClaudeShortAndWeeklyBottlenecks(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	healthy := quotaStandingFor([]quotaWindow{
		standingWindow(now, .7, 5*time.Hour, time.Hour),
		standingWindow(now, .7, 7*24*time.Hour, 6*24*time.Hour),
	}, now)
	healthy.kind = AuthKindOAuth
	for _, tc := range []struct {
		name      string
		short     float64
		weekly    float64
		exhausted bool
	}{
		{"short tight", .98, .2, false},
		{"weekly tight", .2, .98, false},
		{"short exhausted", 1, .2, true},
		{"weekly exhausted", .2, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := quotaStandingFor([]quotaWindow{
				standingWindow(now, tc.short, 5*time.Hour, time.Hour),
				standingWindow(now, tc.weekly, 7*24*time.Hour, time.Hour),
			}, now)
			candidate.kind = AuthKindOAuth
			if candidate.exhausted != tc.exhausted || !quotaStandingLess(healthy, candidate, "healthy", "tight") {
				t.Fatalf("healthy=%+v candidate=%+v", healthy, candidate)
			}
		})
	}
}
