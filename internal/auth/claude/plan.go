package claude

import (
	"strings"

	log "github.com/sirupsen/logrus"
)

// PlanTypeMetadataKey caches the consumer subscription tier on a Claude credential.
const PlanTypeMetadataKey = "plan_type"

// PlanType derives the consumer subscription tier the same way Claude Code does:
// organization_type names the product and rate_limit_tier separates Max 5x from Max 20x.
// Team and Enterprise organizations can carry a Max rate tier, so the tier alone is not a plan.
// The account flags are a fallback for responses that omit organization_type.
// It returns "pro", "max_5x", "max_20x", or "" when the tier is not a consumer plan or cannot be read.
func (p *OAuthProfile) PlanType() string {
	if p == nil {
		return ""
	}
	plan := p.consumerPlanType()
	if plan == "" {
		// Profile strings are unconstrained response data, so log only that mapping failed.
		log.Debug("Claude OAuth profile tier has no consumer plan")
	}
	return plan
}

func (p *OAuthProfile) consumerPlanType() string {
	switch strings.TrimSpace(p.Organization.OrganizationType) {
	case "claude_max":
		return maxPlanType(p.Organization.RateLimitTier)
	case "claude_pro":
		return "pro"
	case "":
		if p.Account.HasClaudeMax {
			return maxPlanType(p.Organization.RateLimitTier)
		}
		if p.Account.HasClaudePro {
			return "pro"
		}
	}
	return ""
}

func maxPlanType(rateLimitTier string) string {
	switch strings.TrimSpace(rateLimitTier) {
	case "default_claude_max_20x":
		return "max_20x"
	case "default_claude_max_5x":
		return "max_5x"
	}
	return ""
}

// StorePlanType records a plan read from a successful profile lookup, clearing a stale value when empty.
func StorePlanType(metadata *map[string]any, plan string) {
	if metadata == nil {
		return
	}
	claudeDevicePoolMu.Lock()
	defer claudeDevicePoolMu.Unlock()
	if plan == "" {
		if *metadata != nil {
			delete(*metadata, PlanTypeMetadataKey)
		}
		return
	}
	if *metadata == nil {
		*metadata = make(map[string]any)
	}
	(*metadata)[PlanTypeMetadataKey] = plan
}

// IsOAuthAccessToken reports whether a Claude credential is a subscription OAuth token rather than an API key.
func IsOAuthAccessToken(token string) bool {
	return strings.Contains(token, "sk-ant-oat")
}
