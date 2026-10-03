package traceflow

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usageidentity"
)

func TestSharedAccountIdentityVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/account-identity-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Secrets map[string]string `json:"installation_secrets_hex"`
		Cases   []struct {
			Name, Installation, Coverage, ID, Ref string
			Evidence                              struct {
				Family          string `json:"family"`
				ProviderKey     string `json:"provider_key"`
				AuthID          string `json:"auth_id"`
				ProviderAccount struct {
					WorkspaceID      string `json:"workspace_account_id"`
					MemberID         string `json:"member_user_id"`
					OrganizationUUID string `json:"organization_uuid"`
					AccountUUID      string `json:"account_uuid"`
				} `json:"provider_account"`
			} `json:"evidence"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			secret, err := hex.DecodeString(fixture.Secrets[test.Installation])
			if err != nil {
				t.Fatal(err)
			}
			evidence := test.Evidence
			executorType := map[string]string{"codex": "CodexExecutor", "claude": "ClaudeExecutor", "gemini": "GeminiExecutor", "openai-compatibility": "OpenAICompatExecutor"}[evidence.Family]
			identity := usageidentity.NewSelected(executorType, evidence.ProviderKey, evidence.AuthID, evidence.ProviderAccount.WorkspaceID, evidence.ProviderAccount.MemberID, evidence.ProviderAccount.OrganizationUUID, evidence.ProviderAccount.AccountUUID)
			coverage, ref := accountReference(secret, identity, evidence.Family)
			if coverage != test.Coverage || ref != test.Ref {
				t.Fatalf("coverage/ref mismatch: %s %s", coverage, ref)
			}
			if coverage != "unknown" {
				var parts []string
				if coverage == "provider-account" && evidence.Family == "codex" {
					parts = []string{"provider-account/1", "codex", identity.WorkspaceID(), identity.MemberID()}
				} else if coverage == "provider-account" {
					parts = []string{"provider-account/1", "claude", identity.OrganizationUUID(), identity.AccountUUID()}
				} else {
					parts = []string{"credential/1", evidence.Family, identity.ProviderKey(), identity.AuthID()}
				}
				id, err := jsArray(parts)
				if err != nil {
					t.Fatal(err)
				}
				if id != test.ID {
					t.Fatalf("id bytes mismatch: %q != %q", id, test.ID)
				}
			}
		})
	}
}
