package management

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func unsignedIDToken(claimsJSON string) string {
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString([]byte(claimsJSON)) + ".sig"
}

func TestAuthSubscriptionPlan(t *testing.T) {
	cases := []struct {
		name string
		auth *coreauth.Auth
		want string
	}{
		{
			name: "codex plan from id token claim",
			auth: &coreauth.Auth{Provider: "codex", Metadata: map[string]any{
				"id_token": unsignedIDToken(`{"https://api.openai.com/auth":{"chatgpt_plan_type":"Pro"}}`),
			}},
			want: "pro",
		},
		{
			name: "codex without plan claim is not reported as free",
			auth: &coreauth.Auth{Provider: "codex", Metadata: map[string]any{
				"id_token": unsignedIDToken(`{"https://api.openai.com/auth":{}}`),
			}},
			want: "",
		},
		{
			name: "codex with unreadable id token",
			auth: &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"id_token": "not-a-jwt"}},
			want: "",
		},
		{
			name: "claude cached plan",
			auth: &coreauth.Auth{Provider: "claude", Metadata: map[string]any{"plan_type": "max_20x"}},
			want: "max_20x",
		},
		{
			name: "claude without cached plan",
			auth: &coreauth.Auth{Provider: "claude", Metadata: map[string]any{}},
			want: "",
		},
		{
			name: "other providers have no plan",
			auth: &coreauth.Auth{Provider: "gemini-cli", Metadata: map[string]any{"plan_type": "pro"}},
			want: "",
		},
		{
			name: "nil auth",
			auth: nil,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := authSubscriptionPlan(tc.auth); got != tc.want {
				t.Fatalf("authSubscriptionPlan() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildAuthFileEntryReportsPlan(t *testing.T) {
	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	entryFor := func(name string, metadata map[string]any) map[string]any {
		path := filepath.Join(authDir, name)
		if errWrite := os.WriteFile(path, []byte(`{"type":"claude"}`), 0o600); errWrite != nil {
			t.Fatalf("write auth file: %v", errWrite)
		}
		auth := &coreauth.Auth{
			ID:         name,
			FileName:   name,
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			Metadata:   metadata,
			Attributes: map[string]string{"path": path},
		}
		entry := h.buildAuthFileEntry(auth)
		if entry == nil {
			t.Fatalf("entry for %s is nil", name)
		}
		return entry
	}

	if got := entryFor("with-plan.json", map[string]any{"plan_type": "max_20x"})["plan"]; got != "max_20x" {
		t.Fatalf("plan = %#v, want max_20x", got)
	}
	if got, ok := entryFor("without-plan.json", map[string]any{})["plan"]; ok {
		t.Fatalf("plan present without a known plan: %#v", got)
	}
}
