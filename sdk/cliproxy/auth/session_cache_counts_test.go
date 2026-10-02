package auth

import (
	"testing"
	"time"
)

func TestSessionCacheActiveCountsAndIdleExpiry(t *testing.T) {
	cache := NewSessionCache(8 * time.Hour)
	defer cache.Stop()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	cache.nowFunc = func() time.Time { return now }

	cache.SetAliases("a", "claude::session-one::model-a", "claude::alias-one::model-a")
	cache.Set("claude::session-one::model-b", "a")
	cache.Set("claude::session-two::model-a", "b")
	if counts := cache.ActiveSessionCounts(30 * time.Minute); counts["a"] != 1 || counts["b"] != 1 {
		t.Fatalf("initial distinct counts = %v, want one per auth", counts)
	}
	if _, ok := cache.GetAndRefresh("claude::alias-one::model-a"); !ok {
		t.Fatal("expected alias lookup to succeed")
	}
	if counts := cache.ActiveSessionCounts(30 * time.Minute); counts["a"] != 1 {
		t.Fatalf("alias-first refresh counted duplicate model binding: %v", counts)
	}

	now = now.Add(31 * time.Minute)
	if counts := cache.ActiveSessionCounts(30 * time.Minute); len(counts) != 0 {
		t.Fatalf("inactive counts = %v, want empty", counts)
	}
	if _, ok := cache.GetAndRefresh("claude::alias-one::model-a"); !ok {
		t.Fatal("expected alias to refresh its logical session")
	}
	if counts := cache.ActiveSessionCounts(30 * time.Minute); counts["a"] != 1 || counts["b"] != 0 {
		t.Fatalf("refreshed counts = %v, want only a", counts)
	}

	now = now.Add(7*time.Hour + 59*time.Minute)
	if _, ok := cache.Get("claude::alias-one::model-a"); !ok {
		t.Fatal("binding expired before eight idle hours")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := cache.Get("claude::alias-one::model-a"); ok {
		t.Fatal("binding survived more than eight idle hours")
	}
	if counts := cache.ActiveSessionCounts(30 * time.Minute); len(counts) != 0 {
		t.Fatalf("expired counts = %v, want empty", counts)
	}
}
