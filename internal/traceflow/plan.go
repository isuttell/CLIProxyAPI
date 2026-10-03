package traceflow

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// maxSpanAttributes matches Trace Flow's v2 cap: a Codex span with every optional field, including the plan.
const maxSpanAttributes = 33

var codexPlans = map[string]string{
	"free":       "chatgpt_free",
	"plus":       "chatgpt_plus",
	"pro":        "chatgpt_pro",
	"team":       "chatgpt_team",
	"business":   "chatgpt_team",
	"enterprise": "chatgpt_enterprise",
	"edu":        "chatgpt_enterprise",
}

var claudePlans = map[string]string{
	"pro":     "claude_pro",
	"max_5x":  "claude_max_5x",
	"max_20x": "claude_max_20x",
}

// accountPlan returns the Trace Flow plan for a subscription credential, "unknown" when its plan could not
// be read, and "" for API keys and other families, which have no plan and must omit the attribute.
func accountPlan(identity usage.AccountIdentity, family string) string {
	if !identity.IsSubscription() || identity.Family() != family {
		return ""
	}
	var plans map[string]string
	switch family {
	case "codex":
		plans = codexPlans
	case "claude":
		plans = claudePlans
	default:
		return ""
	}
	if plan, ok := plans[strings.ToLower(strings.TrimSpace(identity.Plan()))]; ok {
		return plan
	}
	return "unknown"
}
