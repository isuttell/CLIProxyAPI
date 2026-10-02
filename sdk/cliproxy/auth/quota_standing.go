package auth

import (
	"context"
	"time"
)

const (
	quotaSnapshotStaleAfter    = 30 * time.Minute
	routingActiveSessionWindow = 30 * time.Minute
	uncertainRoutingScoreCap   = 0.50
	sessionHeadroomReserve     = 0.10
	coldHeadroomFloor          = 0.15
	childHeadroomFloor         = 0.20
	weeklyUrgencyWeight        = 0.50
	headroomThresholdEpsilon   = 1e-9
)

type quotaWindow struct {
	utilization float64
	resetAt     time.Time
	length      time.Duration
	observedAt  time.Time
}

type quotaStanding struct {
	known     bool
	fresh     bool
	exhausted bool
	headroom  float64
	urgency   float64
	resetAt   time.Time
	kind      string
	active    int
}

// quotaStandingFor uses every live window as a capacity constraint. A stale
// snapshot or elapsed window can cap confidence but cannot claim fresh capacity.
func quotaStandingFor(windows []quotaWindow, now time.Time) quotaStanding {
	var standing quotaStanding
	var longest quotaWindow
	var uncertain bool
	for _, window := range windows {
		if !window.resetAt.After(now) {
			uncertain = true
			continue
		}
		remaining := clampUnit(1 - window.utilization)
		if !standing.known || remaining < standing.headroom {
			standing.headroom = remaining
		}
		standing.known = true
		if window.observedAt.IsZero() || window.observedAt.After(now) || now.Sub(window.observedAt) > quotaSnapshotStaleAfter {
			uncertain = true
		}
		if remaining == 0 {
			standing.exhausted = true
			if window.resetAt.After(standing.resetAt) {
				standing.resetAt = window.resetAt
			}
		}
		if window.length >= 24*time.Hour && window.length > longest.length {
			longest = window
		}
	}
	standing.fresh = standing.known && !uncertain
	if longest.length > 0 && standing.fresh {
		elapsedFraction := 1 - longest.resetAt.Sub(now).Seconds()/longest.length.Seconds()
		standing.urgency = clampUnit((1 - longest.utilization) * clampUnit(elapsedFraction))
	}
	return standing
}

func quotaStandingForSnapshot(windows []quotaWindow, problems []string, now time.Time) quotaStanding {
	standing := quotaStandingFor(windows, now)
	if len(problems) > 0 {
		standing.fresh = false
		standing.urgency = 0
	}
	return standing
}

func clampUnit(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func (s quotaStanding) effectiveHeadroom(active int) float64 {
	if active < 0 {
		active = 0
	}
	return clampUnit(s.headroom - float64(active)*sessionHeadroomReserve)
}

func (s quotaStanding) tier() int {
	if s.exhausted {
		return 4
	}
	if s.kind == AuthKindOAuth {
		if !s.fresh {
			return 1
		}
		if s.effectiveHeadroom(s.active)+headroomThresholdEpsilon >= coldHeadroomFloor {
			return 0
		}
		return 2
	}
	// Unknown kinds do not acquire subscription precedence by inference.
	return 3
}

func quotaStandingLess(a, b quotaStanding, aID, bID string) bool {
	if a.tier() != b.tier() {
		return a.tier() < b.tier()
	}
	aScore := a.routingScore()
	bScore := b.routingScore()
	if aScore != bScore {
		return aScore > bScore
	}
	if a.active != b.active {
		return a.active < b.active
	}
	return aID < bID
}

func (s quotaStanding) routingScore() float64 {
	if s.fresh {
		return s.effectiveHeadroom(s.active) + weeklyUrgencyWeight*s.urgency
	}
	// This is a conservative selection score, never a reported quota balance.
	base := uncertainRoutingScoreCap
	if s.known && s.headroom < base {
		base = s.headroom
	}
	return clampUnit(base - float64(s.active)*sessionHeadroomReserve)
}

type assignedSessionsKey struct{}

func withAssignedSessions(ctx context.Context, counts map[string]int) context.Context {
	return context.WithValue(ctx, assignedSessionsKey{}, counts)
}

func assignedSessionsFrom(ctx context.Context) map[string]int {
	if ctx == nil {
		return nil
	}
	counts, _ := ctx.Value(assignedSessionsKey{}).(map[string]int)
	return counts
}
