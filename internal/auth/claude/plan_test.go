package claude

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

// profileShape mirrors the fields Claude Code 2.1.288 reads from GET /api/oauth/profile
// (organization.organization_type, organization.rate_limit_tier, account.has_claude_max/pro)
// with synthetic values. It is not a captured response.
const profileShape = `{
	"account": {"uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "email": "user@example.com", "display_name": "Example",
		"has_claude_max": true, "has_claude_pro": false},
	"organization": {"uuid": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "name": "Example Org",
		"organization_type": "claude_max", "rate_limit_tier": "default_claude_max_20x",
		"seat_tier": null, "has_extra_usage_enabled": false, "billing_type": "stripe_subscription"}
}`

func TestFetchOAuthProfileReadsPlan(t *testing.T) {
	auth := &ClaudeAuth{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(profileShape)), Header: make(http.Header), Request: req}, nil
	})}}
	profile, err := auth.FetchOAuthProfile(context.Background(), "test-access")
	if err != nil {
		t.Fatalf("FetchOAuthProfile() error = %v", err)
	}
	if got := profile.PlanType(); got != "max_20x" {
		t.Fatalf("PlanType() = %q, want max_20x", got)
	}
}

func TestProfilePlanType(t *testing.T) {
	for _, test := range []struct {
		name, orgType, tier string
		hasMax, hasPro      bool
		want                string
	}{
		{"max 20x", "claude_max", "default_claude_max_20x", true, false, "max_20x"},
		{"max 5x", "claude_max", "default_claude_max_5x", true, false, "max_5x"},
		{"max with unrecognized tier", "claude_max", "default_claude_max_40x", true, false, ""},
		{"pro", "claude_pro", "default_claude_ai", false, true, "pro"},
		{"team seat with max tier is not a consumer plan", "claude_team", "default_claude_max_5x", false, false, ""},
		{"enterprise", "claude_enterprise", "", false, false, ""},
		{"account flags fallback max", "", "default_claude_max_5x", true, false, "max_5x"},
		{"account flags fallback pro", "", "", false, true, "pro"},
		{"nothing", "", "", false, false, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := &OAuthProfile{}
			profile.Organization.OrganizationType = test.orgType
			profile.Organization.RateLimitTier = test.tier
			profile.Account.HasClaudeMax = test.hasMax
			profile.Account.HasClaudePro = test.hasPro
			if got := profile.PlanType(); got != test.want {
				t.Fatalf("PlanType() = %q, want %q", got, test.want)
			}
		})
	}
	if got := (*OAuthProfile)(nil).PlanType(); got != "" {
		t.Fatalf("nil profile PlanType() = %q", got)
	}
}

func TestUpdateTokenStoragePlanFollowsProfileReads(t *testing.T) {
	auth := &ClaudeAuth{}
	storage := &ClaudeTokenStorage{PlanType: "max_20x"}
	auth.UpdateTokenStorage(storage, &ClaudeTokenData{AccessToken: "a"})
	if storage.PlanType != "max_20x" {
		t.Fatalf("failed profile lookup cleared plan: %q", storage.PlanType)
	}
	auth.UpdateTokenStorage(storage, &ClaudeTokenData{AccessToken: "a", PlanType: "max_5x", ProfileRead: true})
	if storage.PlanType != "max_5x" {
		t.Fatalf("profile plan not applied: %q", storage.PlanType)
	}
	auth.UpdateTokenStorage(storage, &ClaudeTokenData{AccessToken: "a", ProfileRead: true})
	if storage.PlanType != "" {
		t.Fatalf("profile without a consumer plan kept stale plan: %q", storage.PlanType)
	}
}

func TestStorePlanType(t *testing.T) {
	var metadata map[string]any
	StorePlanType(&metadata, "pro")
	if metadata[PlanTypeMetadataKey] != "pro" {
		t.Fatalf("plan not stored: %#v", metadata)
	}
	StorePlanType(&metadata, "")
	if _, ok := metadata[PlanTypeMetadataKey]; ok {
		t.Fatal("empty plan did not clear the cached value")
	}
}

func TestPlanTypeLogOmitsProfileStrings(t *testing.T) {
	logger := log.StandardLogger()
	previousLevel, previousOut := logger.GetLevel(), logger.Out
	defer func() { logger.SetLevel(previousLevel); logger.SetOutput(previousOut) }()
	logger.SetLevel(log.DebugLevel)
	var captured strings.Builder
	logger.SetOutput(&captured)

	profile := &OAuthProfile{}
	profile.Organization.OrganizationType = "secret-org-type-alice@example.com"
	profile.Organization.RateLimitTier = "secret-tier-acme-corp"
	if plan := profile.PlanType(); plan != "" {
		t.Fatalf("unmapped tier produced plan %q", plan)
	}
	if out := captured.String(); strings.Contains(out, "secret-") || !strings.Contains(out, "no consumer plan") {
		t.Fatalf("debug log leaked profile strings or was missing: %q", out)
	}
}
