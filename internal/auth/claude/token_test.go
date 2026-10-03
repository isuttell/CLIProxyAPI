package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveTokenToFile_PreservesCustomMetadata(t *testing.T) {
	tempDir := t.TempDir()
	authFilePath := filepath.Join(tempDir, "claude-test.json")

	storage := &ClaudeTokenStorage{
		Type:         "claude",
		Email:        "user@example.com",
		AccessToken:  "new-claude-access",
		RefreshToken: "new-claude-refresh",
		Expire:       "2026-12-31T23:59:59Z",
		LastRefresh:  "2026-04-14T12:00:00Z",
	}
	storage.SetMetadata(map[string]any{
		"disabled":  false,
		"prefix":    "claude-prefix",
		"note":      "claude custom note",
		"proxy_url": "http://proxy:8080",
		"weight":    float64(5),
	})

	if errSave := storage.SaveTokenToFile(authFilePath); errSave != nil {
		t.Fatalf("SaveTokenToFile() error = %v", errSave)
	}

	savedRaw, errRead := os.ReadFile(authFilePath)
	if errRead != nil {
		t.Fatalf("os.ReadFile error = %v", errRead)
	}

	var saved map[string]any
	if errUnmarshal := json.Unmarshal(savedRaw, &saved); errUnmarshal != nil {
		t.Fatalf("json.Unmarshal error = %v", errUnmarshal)
	}

	if saved["access_token"] != "new-claude-access" {
		t.Errorf("access_token = %v, want new-claude-access", saved["access_token"])
	}
	if saved["prefix"] != "claude-prefix" {
		t.Errorf("prefix = %v, want claude-prefix", saved["prefix"])
	}
	if saved["note"] != "claude custom note" {
		t.Errorf("note = %v, want claude custom note", saved["note"])
	}
	if saved["proxy_url"] != "http://proxy:8080" {
		t.Errorf("proxy_url = %v, want http://proxy:8080", saved["proxy_url"])
	}
	if saved["weight"] != float64(5) {
		t.Errorf("weight = %v, want 5", saved["weight"])
	}
}

func TestSaveTokenToFileDoesNotAcceptHookIdentityProvenance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.json")
	storage := &ClaudeTokenStorage{}
	storage.SetMetadata(map[string]any{"identity_provenance": "anthropic_oauth"})
	if err := storage.SaveTokenToFile(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if _, exists := saved["identity_provenance"]; exists {
		t.Fatal("hook metadata minted provider identity evidence")
	}
}

func TestSaveTokenToFilePreservesVerifiedIdentityAgainstHooks(t *testing.T) {
	storage := &ClaudeTokenStorage{AccountUUID: "oauth-account", OrganizationUUID: "oauth-org", IdentityProvenance: "anthropic_oauth"}
	storage.SetMetadata(map[string]any{"account_uuid": "hook-account", "organization_uuid": "hook-org"})
	path := filepath.Join(t.TempDir(), "claude.json")
	if err := storage.SaveTokenToFile(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved ClaudeTokenStorage
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.AccountUUID != storage.AccountUUID || saved.OrganizationUUID != storage.OrganizationUUID || saved.IdentityProvenance != storage.IdentityProvenance {
		t.Fatal("hook metadata changed verified OAuth identity")
	}
}

func TestVerifiedRefreshIdentityPersists(t *testing.T) {
	storage := &ClaudeTokenStorage{AccountUUID: "old-account", OrganizationUUID: "old-org", IdentityProvenance: "anthropic_oauth"}
	updated := *storage
	(&ClaudeAuth{}).UpdateTokenStorage(&updated, &ClaudeTokenData{AccountUUID: "new-account", OrganizationUUID: "new-org", IdentityProvenance: "anthropic_oauth"})
	updated.SetMetadata(map[string]any{"account_uuid": "new-account", "organization_uuid": "new-org", "identity_provenance": "anthropic_oauth"})
	path := filepath.Join(t.TempDir(), "refreshed.json")
	if err := updated.SaveTokenToFile(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved ClaudeTokenStorage
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.AccountUUID != "new-account" || saved.OrganizationUUID != "new-org" || saved.IdentityProvenance != "anthropic_oauth" {
		t.Fatal("verified refresh reverted on disk")
	}
	if storage.AccountUUID != "old-account" {
		t.Fatal("mutated shared login storage")
	}
}
