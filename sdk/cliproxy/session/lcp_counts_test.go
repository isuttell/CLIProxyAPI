package session

import (
	"testing"
	"time"
)

func TestMerklePrefixMatcherActiveSessionCounts(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	matcher := NewMerklePrefixMatcherWithConfig(MerklePrefixMatcherConfig{
		TTL:     8 * time.Hour,
		NowFunc: func() time.Time { return now },
	})
	first := matcher.BindFingerprints("scope-one", []string{"user-one", "answer"}, 1, "auth-a")
	continued := matcher.BindFingerprints("scope-one", []string{"user-one", "answer", "followup"}, 1, "auth-a")
	if first == "" || continued != first {
		t.Fatalf("continuation session = %q, want %q", continued, first)
	}
	matcher.BindFingerprints("scope-two", []string{"user-two"}, 1, "auth-a")
	matcher.BindFingerprints("scope-one", []string{"user-three"}, 1, "auth-b")
	if counts := matcher.ActiveSessionCounts(30 * time.Minute); counts["auth-a"] != 2 || counts["auth-b"] != 1 {
		t.Fatalf("active distinct counts = %v, want a=2 b=1", counts)
	}

	now = now.Add(31 * time.Minute)
	if counts := matcher.ActiveSessionCounts(30 * time.Minute); len(counts) != 0 {
		t.Fatalf("inactive counts = %v, want empty", counts)
	}
	matcher.TouchFingerprints("scope-one", []string{"user-one", "answer", "followup"}, 1, "auth-a")
	if counts := matcher.ActiveSessionCounts(30 * time.Minute); counts["auth-a"] != 1 || counts["auth-b"] != 0 {
		t.Fatalf("refreshed counts = %v, want only a=1", counts)
	}

	now = now.Add(8*time.Hour + time.Minute)
	if counts := matcher.ActiveSessionCounts(30 * time.Minute); len(counts) != 0 {
		t.Fatalf("expired counts = %v, want empty", counts)
	}
}
