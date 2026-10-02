package auth

import (
	"context"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// ExpiringFirstSelector spends subscription headroom while reserving capacity for
// active sessions. Explicit credential priority is applied before this ranking.
type ExpiringFirstSelector struct {
	mu              sync.Mutex
	warnedSnapshots map[string]time.Time
}

// Pick selects from the highest available operator priority before applying quota tiers.
func (s *ExpiringFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	counts := assignedSessionsFrom(ctx)
	var best *Auth
	var bestStanding quotaStanding
	var earliestReset time.Time
	for _, auth := range available {
		windows, problems := authQuotaWindows(auth, model)
		s.warnUnparsable(auth, problems)
		standing := quotaStandingForSnapshot(windows, problems, now)
		standing.active = counts[auth.ID]
		standing.kind = auth.AuthKind()
		if standing.exhausted && (earliestReset.IsZero() || standing.resetAt.Before(earliestReset)) {
			earliestReset = standing.resetAt
		}
		if best == nil || quotaStandingLess(standing, bestStanding, auth.ID, best.ID) {
			best, bestStanding = auth, standing
		}
	}
	if bestStanding.exhausted {
		return nil, newModelCooldownError(model, provider, earliestReset.Sub(now))
	}
	return best, nil
}

func (s *ExpiringFirstSelector) canInherit(auth *Auth, model string, now time.Time, active int) bool {
	if auth == nil {
		return false
	}
	standing := quotaStandingForAuth(auth, model, now)
	return standing.fresh && !standing.exhausted && standing.effectiveHeadroom(active+1)+headroomThresholdEpsilon >= childHeadroomFloor
}

func quotaStandingForAuth(auth *Auth, model string, now time.Time) quotaStanding {
	windows, problems := authQuotaWindows(auth, model)
	return quotaStandingForSnapshot(windows, problems, now)
}

func (s *ExpiringFirstSelector) warnUnparsable(auth *Auth, problems []string) {
	if len(problems) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if warned, ok := s.warnedSnapshots[auth.ID]; ok && warned.Equal(auth.Quota.ObservedAt) {
		return
	}
	if s.warnedSnapshots == nil {
		s.warnedSnapshots = make(map[string]time.Time)
	}
	s.warnedSnapshots[auth.ID] = auth.Quota.ObservedAt
	log.Warnf("expiring-first: incomplete or invalid quota windows for auth %s: %s", auth.ID, strings.Join(problems, "; "))
}
