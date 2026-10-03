package helps

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func testIdentityToken(workspace, member string) string {
	payload, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]string{
			"chatgpt_account_id": workspace,
			"chatgpt_user_id":    member,
		},
	})
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestCodexAccountSnapshotRequiresSameStoredIDToken(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "credential", Metadata: map[string]any{
		"id_token":   testIdentityToken("workspace", "member-a"),
		"account_id": "different-workspace",
	}}
	original := snapshotAccountIdentity("CodexExecutor", "codex", auth)
	if original.WorkspaceID() != "workspace" || original.MemberID() != "member-a" {
		t.Fatalf("stored ID token identity = %+v", original)
	}
	auth.Metadata["id_token"] = testIdentityToken("workspace", "member-b")
	if original.MemberID() != "member-a" {
		t.Fatal("snapshot changed after credential refresh")
	}
	refreshed := snapshotAccountIdentity("CodexExecutor", "codex", auth)
	if refreshed.MemberID() != "member-b" || refreshed.WorkspaceID() != original.WorkspaceID() {
		t.Fatalf("member separation after refresh = %+v", refreshed)
	}
	auth.Metadata["id_token"] = testIdentityToken("workspace", "")
	missing := snapshotAccountIdentity("CodexExecutor", "codex", auth)
	if missing.WorkspaceID() != "" || missing.MemberID() != "" {
		t.Fatalf("workspace alone must not qualify: %+v", missing)
	}
}

func TestClaudeAccountSnapshotRequiresProvenance(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "credential", Metadata: map[string]any{
		"organization_uuid": "org", "account_uuid": "account",
	}}
	unmarked := snapshotAccountIdentity("ClaudeExecutor", "claude", auth)
	if unmarked.OrganizationUUID() != "" || unmarked.AccountUUID() != "" {
		t.Fatalf("unmarked UUIDs must remain credential coverage: %+v", unmarked)
	}
	auth.Metadata["identity_provenance"] = "anthropic_oauth"
	marked := snapshotAccountIdentity("ClaudeExecutor", "claude", auth)
	if marked.OrganizationUUID() != "org" || marked.AccountUUID() != "account" {
		t.Fatalf("marked OAuth identity = %+v", marked)
	}
}

func TestAccountSnapshotPreservesOpaqueIDsVerbatim(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "credential", Metadata: map[string]any{
		"id_token": testIdentityToken(" workspace ", " member "),
	}}
	codex := snapshotAccountIdentity("CodexExecutor", "codex", auth)
	if codex.WorkspaceID() != " workspace " || codex.MemberID() != " member " {
		t.Fatalf("Codex opaque IDs were changed: %v", codex)
	}
	claudeAuth := &cliproxyauth.Auth{ID: "credential", Metadata: map[string]any{
		"organization_uuid": " org ", "account_uuid": " account ", "identity_provenance": "anthropic_oauth",
	}}
	claude := snapshotAccountIdentity("ClaudeExecutor", "claude", claudeAuth)
	if claude.OrganizationUUID() != " org " || claude.AccountUUID() != " account " {
		t.Fatalf("Claude opaque IDs were changed: %v", claude)
	}
	claudeAuth.Metadata["account_uuid"] = string([]byte{0xff})
	invalid := snapshotAccountIdentity("ClaudeExecutor", "claude", claudeAuth)
	if invalid.OrganizationUUID() != "" || invalid.AccountUUID() != "" {
		t.Fatalf("invalid UTF-8 identity qualified: %v", invalid)
	}
}

func testPlanToken(plan string) string {
	payload, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]string{
			"chatgpt_account_id": "workspace", "chatgpt_user_id": "member", "chatgpt_plan_type": plan,
		},
	})
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestCodexSnapshotCapturesSubscriptionPlan(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "credential", Metadata: map[string]any{"id_token": testPlanToken("plus"), "plan_type": "free"}}
	snapshot := snapshotAccountIdentity("CodexExecutor", "codex", auth)
	if !snapshot.IsSubscription() || snapshot.Plan() != "plus" {
		t.Fatalf("subscription=%v plan=%q, want token plan plus", snapshot.IsSubscription(), snapshot.Plan())
	}
	auth.Metadata["id_token"] = testPlanToken("pro")
	if snapshot.Plan() != "plus" {
		t.Fatal("snapshot plan changed after credential refresh")
	}
	auth.Metadata["id_token"] = testPlanToken("")
	if missing := snapshotAccountIdentity("CodexExecutor", "codex", auth); !missing.IsSubscription() || missing.Plan() != "" {
		t.Fatalf("missing plan claim must stay unread, not default to free: %q", missing.Plan())
	}
	apiKey := &cliproxyauth.Auth{ID: "codex-api-key", Attributes: map[string]string{"api_key": "synthetic-key"}}
	if snapshotAccountIdentity("CodexExecutor", "codex", apiKey).IsSubscription() {
		t.Fatal("API key credential treated as a subscription")
	}
}

func TestCodexOAuthWithoutReadableIDTokenIsSubscriptionWithUnknownPlan(t *testing.T) {
	cases := map[string]map[string]any{
		"absent":    {"access_token": "synthetic-access"},
		"empty":     {"access_token": "synthetic-access", "id_token": ""},
		"malformed": {"access_token": "synthetic-access", "id_token": "not-a-jwt"},
	}
	for name, metadata := range cases {
		auth := &cliproxyauth.Auth{ID: "credential-" + name, Metadata: metadata}
		snapshot := snapshotAccountIdentity("CodexExecutor", "codex", auth)
		if !snapshot.IsSubscription() || snapshot.Plan() != "" {
			t.Fatalf("%s id_token: subscription=%v plan=%q, want subscription with unread plan", name, snapshot.IsSubscription(), snapshot.Plan())
		}
	}
}

func TestClaudeSnapshotCapturesSubscriptionPlan(t *testing.T) {
	oauth := &cliproxyauth.Auth{ID: "credential", Metadata: map[string]any{"access_token": "sk-ant-oat01-synthetic", "plan_type": "max_20x"}}
	snapshot := snapshotAccountIdentity("ClaudeExecutor", "claude", oauth)
	if !snapshot.IsSubscription() || snapshot.Plan() != "max_20x" {
		t.Fatalf("subscription=%v plan=%q, want max_20x", snapshot.IsSubscription(), snapshot.Plan())
	}
	configured := &cliproxyauth.Auth{ID: "config", Attributes: map[string]string{"api_key": "sk-ant-oat01-synthetic"}}
	if got := snapshotAccountIdentity("ClaudeExecutor", "claude", configured); !got.IsSubscription() || got.Plan() != "" {
		t.Fatalf("configured OAuth token: subscription=%v plan=%q, want unread subscription", got.IsSubscription(), got.Plan())
	}
	apiKey := &cliproxyauth.Auth{ID: "api", Attributes: map[string]string{"api_key": "sk-ant-api03-synthetic"}, Metadata: map[string]any{"plan_type": "pro"}}
	if snapshotAccountIdentity("ClaudeExecutor", "claude", apiKey).IsSubscription() {
		t.Fatal("API key credential treated as a subscription")
	}
}
