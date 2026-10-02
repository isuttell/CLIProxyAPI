package auth

import (
	"context"
	"time"
)

func (s *SessionAffinitySelector) assignmentCounts() map[string]int {
	counts := s.cache.ActiveSessionCounts(routingActiveSessionWindow)
	for authID, count := range s.matcher.ActiveSessionCounts(routingActiveSessionWindow) {
		counts[authID] += count
	}
	return counts
}

func (s *SessionAffinitySelector) withAssignmentCounts(ctx context.Context) context.Context {
	if _, ok := s.fallback.(*ExpiringFirstSelector); !ok {
		return ctx
	}
	return withAssignedSessions(ctx, s.assignmentCounts())
}

func (s *SessionAffinitySelector) canInherit(ctx context.Context, auth *Auth, model string, now time.Time) bool {
	selector, ok := s.fallback.(*ExpiringFirstSelector)
	if !ok {
		return true
	}
	return selector.canInherit(auth, quotaSelectionModel(ctx, auth, model), now, s.assignmentCounts()[auth.ID])
}

func (s *SessionAffinitySelector) pinUsable(ctx context.Context, auth *Auth, model string, now time.Time) bool {
	if _, ok := s.fallback.(*ExpiringFirstSelector); !ok {
		return true
	}
	return !quotaStandingForAuth(auth, quotaSelectionModel(ctx, auth, model), now).exhausted
}
