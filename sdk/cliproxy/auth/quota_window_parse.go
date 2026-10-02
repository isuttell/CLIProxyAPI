package auth

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	maxQuotaWindowMinutes = 30 * 24 * 60
	maxQuotaResetInterval = 30 * 24 * time.Hour
)

func authQuotaWindows(auth *Auth, model string) ([]quotaWindow, []string) {
	if auth == nil || auth.AuthKind() == AuthKindAPIKey || len(auth.Quota.Signals) == 0 {
		return nil, nil
	}
	signals, observedAt := auth.Quota.Signals, auth.Quota.ObservedAt
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		return claudeQuotaWindows(signals, observedAt)
	case "codex":
		return codexQuotaWindows(signals, observedAt, model)
	default:
		return nil, nil
	}
}

func claudeQuotaWindows(signals map[string]string, observedAt time.Time) ([]quotaWindow, []string) {
	var windows []quotaWindow
	var problems []string
	for _, claim := range []struct {
		name   string
		length time.Duration
	}{{"5h", 5 * time.Hour}, {"7d", 7 * 24 * time.Hour}} {
		prefix := "Anthropic-Ratelimit-Unified-" + claim.name + "-"
		rawUtilization, okUtilization := signals[prefix+"Utilization"]
		rawReset, okReset := signals[prefix+"Reset"]
		if !okUtilization && !okReset {
			if len(signals) > 0 {
				problems = append(problems, "claude "+claim.name+" missing window")
			}
			continue
		}
		if !okUtilization || !okReset {
			problems = append(problems, "claude "+claim.name+" incomplete window")
			continue
		}
		utilization, errUtilization := strconv.ParseFloat(rawUtilization, 64)
		resetUnix, errReset := strconv.ParseInt(rawReset, 10, 64)
		if errUtilization != nil || !finiteFraction(utilization) || errReset != nil || resetUnix <= 0 || !validAbsoluteReset(time.Unix(resetUnix, 0), observedAt) {
			problems = append(problems, fmt.Sprintf("claude %s utilization=%q reset=%q", claim.name, rawUtilization, rawReset))
			continue
		}
		if strings.EqualFold(signals[prefix+"Status"], "rejected") {
			utilization = 1
		}
		windows = append(windows, quotaWindow{utilization: utilization, resetAt: time.Unix(resetUnix, 0), length: claim.length, observedAt: observedAt})
	}
	return windows, problems
}

func codexQuotaWindows(signals map[string]string, observedAt time.Time, model string) ([]quotaWindow, []string) {
	var windows []quotaWindow
	var problems []string
	for _, prefix := range []string{"X-Codex-Primary-", "X-Codex-Secondary-"} {
		if window, present, valid := parseCodexWindow(signals, prefix, observedAt); present {
			if valid {
				windows = append(windows, window)
			} else {
				problems = append(problems, "codex "+prefix+" window")
			}
		} else if len(signals) > 0 {
			problems = append(problems, "codex "+prefix+" missing window")
		}
	}
	requested := canonicalModelKey(model)
	if requested == "" {
		return windows, problems
	}
	for name, limitModel := range signals {
		if !strings.HasPrefix(name, "X-Codex-") || !strings.HasSuffix(name, "-Limit-Name") || !strings.EqualFold(canonicalModelKey(limitModel), requested) {
			continue
		}
		prefix := strings.TrimSuffix(name, "Limit-Name")
		for _, windowName := range []string{"Primary-", "Secondary-"} {
			if window, present, valid := parseCodexWindow(signals, prefix+windowName, observedAt); present {
				if valid {
					windows = append(windows, window)
				} else {
					problems = append(problems, "codex "+prefix+windowName+" window")
				}
			}
		}
	}
	return windows, problems
}

func parseCodexWindow(signals map[string]string, prefix string, observedAt time.Time) (quotaWindow, bool, bool) {
	rawUsed, okUsed := signals[prefix+"Used-Percent"]
	rawMinutes, okMinutes := signals[prefix+"Window-Minutes"]
	if !okUsed && !okMinutes {
		return quotaWindow{}, false, false
	}
	if !okUsed || !okMinutes {
		return quotaWindow{}, true, false
	}
	used, errUsed := strconv.ParseFloat(rawUsed, 64)
	minutes, errMinutes := strconv.ParseInt(rawMinutes, 10, 64)
	resetAt, okReset := codexWindowResetAt(signals, prefix, observedAt)
	if errUsed != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 100 || errMinutes != nil || minutes <= 0 || minutes > maxQuotaWindowMinutes || !okReset {
		return quotaWindow{}, true, false
	}
	return quotaWindow{utilization: used / 100, resetAt: resetAt, length: time.Duration(minutes) * time.Minute, observedAt: observedAt}, true, true
}

func finiteFraction(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func validAbsoluteReset(resetAt, observedAt time.Time) bool {
	if observedAt.IsZero() {
		return false
	}
	interval := resetAt.Sub(observedAt)
	return interval >= -maxQuotaResetInterval && interval <= maxQuotaResetInterval
}

// Absolute resets take precedence; relative resets must be positive and bounded
// before converting to a duration so malformed headers cannot overflow.
func codexWindowResetAt(signals map[string]string, prefix string, observedAt time.Time) (time.Time, bool) {
	if resetUnix, errParse := strconv.ParseInt(signals[prefix+"Reset-At"], 10, 64); errParse == nil && resetUnix > 0 {
		resetAt := time.Unix(resetUnix, 0)
		if validAbsoluteReset(resetAt, observedAt) {
			return resetAt, true
		}
	}
	seconds, errParse := strconv.ParseInt(signals[prefix+"Reset-After-Seconds"], 10, 64)
	if errParse != nil || seconds <= 0 || seconds > int64(maxQuotaResetInterval/time.Second) || observedAt.IsZero() {
		return time.Time{}, false
	}
	return observedAt.Add(time.Duration(seconds) * time.Second), true
}
