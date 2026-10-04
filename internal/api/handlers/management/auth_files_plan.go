package management

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// authSubscriptionPlan returns the provider-local subscription plan recorded on a
// credential ("pro", "max_20x", ...), or "" when none is known.
//
// Codex reads the raw chatgpt_plan_type claim from the stored ID token rather than
// GetPlanType, whose "free" default would mislabel a credential with no plan.
// Claude reads the plan cached on the credential at login and refresh.
func authSubscriptionPlan(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "codex":
		idToken, _ := auth.Metadata["id_token"].(string)
		if idToken == "" {
			return ""
		}
		claims, errParse := codex.ParseJWTToken(idToken)
		if errParse != nil || claims == nil {
			return ""
		}
		return strings.ToLower(strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType))
	case "claude":
		return strings.ToLower(claude.ReadMetadataString(&auth.Metadata, claude.PlanTypeMetadataKey))
	default:
		return ""
	}
}
