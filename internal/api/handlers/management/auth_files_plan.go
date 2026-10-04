package management

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
)

// isV8Request reports whether the request came through the v8 management routes.
// The v0 contract is frozen, so v8-only response fields are gated on this.
func isV8Request(c *gin.Context) bool {
	return c.GetBool(ConfigV8ContextKey)
}

// addSubscriptionPlan sets entry["plan"] when the credential has a known plan.
func addSubscriptionPlan(entry gin.H, provider string, metadata map[string]any, attributes map[string]string) {
	if entry == nil {
		return
	}
	if plan := subscriptionPlan(provider, metadata, attributes); plan != "" {
		entry["plan"] = plan
	}
}

// subscriptionPlan returns the provider-local plan ("pro", "max_20x", ...) for a
// subscription login, or "" for API keys and credentials whose plan is unknown.
//
// The subscription checks mirror snapshotAccountIdentity in
// internal/runtime/executor/helps/usage_identity.go so the listing and usage
// export agree: a configured API key overrides any OAuth metadata, Codex reads
// the raw chatgpt_plan_type claim (GetPlanType defaults to "free"), and Claude
// reads the cached plan only when the selected credential is an OAuth token.
func subscriptionPlan(provider string, metadata map[string]any, attributes map[string]string) string {
	if metadata == nil {
		return ""
	}
	apiKey := strings.TrimSpace(attributes["api_key"])
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		if apiKey != "" {
			return ""
		}
		idToken, _ := metadata["id_token"].(string)
		if idToken == "" {
			return ""
		}
		claims, errParse := codex.ParseJWTToken(idToken)
		if errParse != nil || claims == nil {
			return ""
		}
		return strings.ToLower(strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType))
	case "claude":
		credential := apiKey
		if credential == "" {
			credential = claude.ReadMetadataString(&metadata, "access_token")
		}
		if !claude.IsOAuthAccessToken(credential) {
			return ""
		}
		return strings.ToLower(claude.ReadMetadataString(&metadata, claude.PlanTypeMetadataKey))
	default:
		return ""
	}
}
