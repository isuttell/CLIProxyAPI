package helps

import (
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usageidentity"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func snapshotAccountIdentity(executorType, provider string, auth *cliproxyauth.Auth) usage.AccountIdentity {
	if auth == nil {
		return usage.AccountIdentity{}
	}
	var workspaceID, memberID, organizationUUID, accountUUID, plan string
	subscription := false
	family := usageidentity.Family(executorType, provider)
	switch family {
	case "codex":
		apiKey := ""
		if auth.Attributes != nil {
			apiKey = auth.Attributes["api_key"]
		}
		storedToken, accessToken := "", ""
		if auth.Metadata != nil {
			storedToken, _ = auth.Metadata["id_token"].(string)
			accessToken, _ = auth.Metadata["access_token"].(string)
		}
		// An OAuth login is a subscription even when its ID token is missing or unreadable;
		// the ID token only supplies the plan, which then reports as unknown.
		subscription = apiKey == "" && (storedToken != "" || accessToken != "")
		if storedToken != "" {
			claims, errParse := codex.ParseJWTToken(storedToken)
			if errParse == nil && claims != nil {
				workspaceID, memberID = claims.CodexAuthInfo.ChatgptAccountID, claims.CodexAuthInfo.ChatgptUserID
				if !validOpaqueID(workspaceID) || !validOpaqueID(memberID) {
					workspaceID, memberID = "", ""
				}
				// The raw claim, not GetPlanType, because its "free" default would mislabel a missing plan.
				plan = claims.CodexAuthInfo.ChatgptPlanType
			}
		}
	case "claude":
		var provenance string
		organizationUUID, accountUUID, provenance = claude.ReadOAuthIdentity(&auth.Metadata)
		if provenance != "anthropic_oauth" || !validOpaqueID(organizationUUID) || !validOpaqueID(accountUUID) {
			organizationUUID, accountUUID = "", ""
		}
		credential := ""
		if auth.Attributes != nil {
			credential = auth.Attributes["api_key"]
		}
		if credential == "" {
			credential = claude.ReadMetadataString(&auth.Metadata, "access_token")
		}
		subscription = claude.IsOAuthAccessToken(credential)
		if subscription {
			plan = claude.ReadMetadataString(&auth.Metadata, claude.PlanTypeMetadataKey)
		}
	}
	identity := usageidentity.NewSelected(executorType, provider, auth.ID, workspaceID, memberID, organizationUUID, accountUUID)
	if subscription {
		identity = identity.WithSubscriptionPlan(plan)
	}
	return identity
}

func validOpaqueID(value string) bool { return value != "" && utf8.ValidString(value) }
