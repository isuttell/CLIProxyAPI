package management

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func unsignedIDToken(claimsJSON string) string {
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString([]byte(claimsJSON)) + ".sig"
}

func TestSubscriptionPlan(t *testing.T) {
	proToken := unsignedIDToken(`{"https://api.openai.com/auth":{"chatgpt_plan_type":"Pro"}}`)
	cases := []struct {
		name       string
		provider   string
		metadata   map[string]any
		attributes map[string]string
		want       string
	}{
		{"codex plan from id token claim", "codex", map[string]any{"id_token": proToken}, nil, "pro"},
		{"codex missing claim is not reported as free", "codex", map[string]any{"id_token": unsignedIDToken(`{"https://api.openai.com/auth":{}}`)}, nil, ""},
		{"codex unreadable id token", "codex", map[string]any{"id_token": "not-a-jwt"}, nil, ""},
		{"codex api key overrides oauth metadata", "codex", map[string]any{"id_token": proToken}, map[string]string{"api_key": "sk-proj-synthetic"}, ""},
		{"claude oauth cached plan", "claude", map[string]any{"access_token": "sk-ant-oat01-synthetic", "plan_type": "max_20x"}, nil, "max_20x"},
		{"claude oauth without cached plan", "claude", map[string]any{"access_token": "sk-ant-oat01-synthetic"}, nil, ""},
		{"claude api key ignores stale plan", "claude", map[string]any{"plan_type": "pro"}, map[string]string{"api_key": "sk-ant-api03-synthetic"}, ""},
		{"claude configured oauth token reads plan", "claude", map[string]any{"plan_type": "pro"}, map[string]string{"api_key": "sk-ant-oat01-synthetic"}, "pro"},
		{"other providers have no plan", "gemini-cli", map[string]any{"plan_type": "pro"}, nil, ""},
		{"nil metadata", "claude", nil, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := subscriptionPlan(tc.provider, tc.metadata, tc.attributes); got != tc.want {
				t.Fatalf("subscriptionPlan() = %q, want %q", got, tc.want)
			}
		})
	}
}

func listAuthFilesForPlanTest(t *testing.T, h *Handler, v8 bool) []map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/management/credentials", nil)
	if v8 {
		ctx.Set(ConfigV8ContextKey, true)
	}
	h.ListAuthFiles(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("files = %s, want one entry", rec.Body.String())
	}
	return payload.Files
}

const claudeAuthFileWithPlan = `{"type":"claude","access_token":"sk-ant-oat01-synthetic","plan_type":"max_20x"}`

func TestListAuthFilesReportsPlanOnlyOnV8(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	path := filepath.Join(authDir, "claude.json")
	if errWrite := os.WriteFile(path, []byte(claudeAuthFileWithPlan), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	registerAuthForLookupTest(t, manager, &coreauth.Auth{
		ID:         "claude.json",
		FileName:   "claude.json",
		Provider:   "claude",
		Status:     coreauth.StatusActive,
		Metadata:   map[string]any{"access_token": "sk-ant-oat01-synthetic", "plan_type": "max_20x"},
		Attributes: map[string]string{"path": path},
	})
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	if got := listAuthFilesForPlanTest(t, h, true)[0]["plan"]; got != "max_20x" {
		t.Fatalf("v8 plan = %#v, want max_20x", got)
	}
	if got, ok := listAuthFilesForPlanTest(t, h, false)[0]["plan"]; ok {
		t.Fatalf("v0 response gained plan: %#v", got)
	}
}

func TestListAuthFilesFromDiskReportsPlanOnlyOnV8(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	if errWrite := os.WriteFile(filepath.Join(authDir, "claude.json"), []byte(claudeAuthFileWithPlan), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)

	if got := listAuthFilesForPlanTest(t, h, true)[0]["plan"]; got != "max_20x" {
		t.Fatalf("v8 disk plan = %#v, want max_20x", got)
	}
	if got, ok := listAuthFilesForPlanTest(t, h, false)[0]["plan"]; ok {
		t.Fatalf("v0 disk response gained plan: %#v", got)
	}
}
