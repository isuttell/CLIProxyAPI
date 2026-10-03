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
	var workspaceID, memberID, organizationUUID, accountUUID string
	switch usageidentity.Family(executorType, provider) {
	case "codex":
		if auth.Metadata != nil {
			storedToken, _ := auth.Metadata["id_token"].(string)
			if storedToken != "" {
				claims, errParse := codex.ParseJWTToken(storedToken)
				if errParse == nil && claims != nil {
					workspaceID, memberID = claims.CodexAuthInfo.ChatgptAccountID, claims.CodexAuthInfo.ChatgptUserID
					if !validOpaqueID(workspaceID) || !validOpaqueID(memberID) {
						workspaceID, memberID = "", ""
					}
				}
			}
		}
	case "claude":
		var provenance string
		organizationUUID, accountUUID, provenance = claude.ReadOAuthIdentity(&auth.Metadata)
		if provenance != "anthropic_oauth" || !validOpaqueID(organizationUUID) || !validOpaqueID(accountUUID) {
			organizationUUID, accountUUID = "", ""
		}
	}
	return usageidentity.NewSelected(executorType, provider, auth.ID, workspaceID, memberID, organizationUUID, accountUUID)
}

func validOpaqueID(value string) bool { return value != "" && utf8.ValidString(value) }
