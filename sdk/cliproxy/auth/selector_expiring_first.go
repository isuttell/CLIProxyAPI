package auth

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// ExpiringFirstSelector drains the subscription credential whose quota window resets
// soonest. Capacity left in a window is lost when it resets, so serving traffic from the
// earliest-resetting credential first wastes the least subscription quota overall.
//
// Rankings come from the passive quota snapshot (Auth.Quota.Signals) captured from Claude
// and Codex response headers. Credentials without a live snapshot rank after credentials
// with one: an unused subscription has no window running, so nothing on it is expiring.
// Credentials with an exhausted window rank last. Remaining ties fall back to ID order,
// which makes providers without quota headers behave like fill-first.
type ExpiringFirstSelector struct {
	mu sync.Mutex
	// warnedSnapshots records the snapshot each auth was last warned about, so a malformed
	// header is reported once instead of on every pick until the next response replaces it.
	warnedSnapshots map[string]time.Time
}

// quotaWindow is one rolling subscription limit parsed from a quota snapshot.
type quotaWindow struct {
	utilization float64
	resetAt     time.Time
	length      time.Duration
}

// quotaStanding summarizes how urgently a credential's remaining quota should be used.
type quotaStanding struct {
	known       bool
	exhausted   bool
	resetAt     time.Time
	utilization float64
}

// Pick selects the available auth whose longest live quota window resets first.
func (s *ExpiringFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	var best *Auth
	var bestStanding quotaStanding
	for _, auth := range available {
		windows, problems := authQuotaWindows(auth)
		s.warnUnparsable(auth, problems)
		standing := quotaStandingFor(windows, now)
		if best == nil || quotaStandingLess(standing, bestStanding, auth.ID, best.ID) {
			best, bestStanding = auth, standing
		}
	}
	return best, nil
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
	log.Warnf("expiring-first: ignoring unparsable quota windows for auth %s: %s", auth.ID, strings.Join(problems, "; "))
}

func quotaStandingLess(a, b quotaStanding, aID, bID string) bool {
	if a.exhausted != b.exhausted {
		return !a.exhausted
	}
	if a.known != b.known {
		return a.known
	}
	if a.known && !a.resetAt.Equal(b.resetAt) {
		return a.resetAt.Before(b.resetAt)
	}
	if a.utilization != b.utilization {
		return a.utilization > b.utilization
	}
	return aID < bID
}

// quotaStandingFor ranks a credential by its longest live window, since the long window
// (weekly on current Claude and Codex plans) bounds how much quota can still be wasted.
// Account-wide flags (Claude Unified-Status, Codex Limit-Reached) are deliberately ignored:
// they carry no reset time, so once the short window behind them resets they would keep
// burying the credential, and a live exhaustion already shows in the per-window data.
func quotaStandingFor(windows []quotaWindow, now time.Time) quotaStanding {
	var standing quotaStanding
	var longest quotaWindow
	for _, window := range windows {
		if !window.resetAt.After(now) {
			continue
		}
		if window.utilization >= 1 {
			standing.exhausted = true
		}
		if !standing.known || window.length > longest.length {
			longest = window
			standing.known = true
		}
	}
	standing.resetAt = longest.resetAt
	standing.utilization = longest.utilization
	return standing
}

func authQuotaWindows(auth *Auth) ([]quotaWindow, []string) {
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return nil, nil
	}
	signals := auth.Quota.Signals
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		return claudeQuotaWindows(signals)
	case "codex":
		return codexQuotaWindows(signals, auth.Quota.ObservedAt)
	default:
		return nil, nil
	}
}

func claudeQuotaWindows(signals map[string]string) ([]quotaWindow, []string) {
	var windows []quotaWindow
	var problems []string
	for _, claim := range []struct {
		name   string
		length time.Duration
	}{
		{name: "5h", length: 5 * time.Hour},
		{name: "7d", length: 7 * 24 * time.Hour},
	} {
		prefix := "Anthropic-Ratelimit-Unified-" + claim.name + "-"
		rawUtilization, okUtilization := signals[prefix+"Utilization"]
		rawReset, okReset := signals[prefix+"Reset"]
		if !okUtilization || !okReset {
			continue
		}
		utilization, errUtilization := strconv.ParseFloat(rawUtilization, 64)
		resetUnix, errReset := strconv.ParseInt(rawReset, 10, 64)
		if errUtilization != nil || errReset != nil {
			problems = append(problems, fmt.Sprintf("claude %s utilization=%q reset=%q", claim.name, rawUtilization, rawReset))
			continue
		}
		if strings.EqualFold(signals[prefix+"Status"], "rejected") {
			utilization = 1
		}
		windows = append(windows, quotaWindow{utilization: utilization, resetAt: time.Unix(resetUnix, 0), length: claim.length})
	}
	return windows, problems
}

func codexQuotaWindows(signals map[string]string, observedAt time.Time) ([]quotaWindow, []string) {
	var windows []quotaWindow
	var problems []string
	for _, name := range []string{"Primary", "Secondary"} {
		prefix := "X-Codex-" + name + "-"
		rawUsed, okUsed := signals[prefix+"Used-Percent"]
		rawMinutes, okMinutes := signals[prefix+"Window-Minutes"]
		if !okUsed || !okMinutes {
			continue
		}
		used, errUsed := strconv.ParseFloat(rawUsed, 64)
		minutes, errMinutes := strconv.ParseInt(rawMinutes, 10, 64)
		resetAt, okResetAt := codexWindowResetAt(signals, prefix, observedAt)
		if errUsed != nil || errMinutes != nil || !okResetAt {
			problems = append(problems, fmt.Sprintf("codex %s used=%q minutes=%q reset-at=%q reset-after=%q",
				strings.ToLower(name), rawUsed, rawMinutes, signals[prefix+"Reset-At"], signals[prefix+"Reset-After-Seconds"]))
			continue
		}
		windows = append(windows, quotaWindow{utilization: used / 100, resetAt: resetAt, length: time.Duration(minutes) * time.Minute})
	}
	return windows, problems
}

// codexWindowResetAt prefers the absolute reset timestamp and falls back to the relative
// form, anchored at the time the snapshot was observed.
func codexWindowResetAt(signals map[string]string, prefix string, observedAt time.Time) (time.Time, bool) {
	if resetUnix, errParse := strconv.ParseInt(signals[prefix+"Reset-At"], 10, 64); errParse == nil {
		return time.Unix(resetUnix, 0), true
	}
	seconds, errParse := strconv.ParseInt(signals[prefix+"Reset-After-Seconds"], 10, 64)
	if errParse != nil || observedAt.IsZero() {
		return time.Time{}, false
	}
	return observedAt.Add(time.Duration(seconds) * time.Second), true
}
