package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func explicitSessionOptions(id string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		Headers:  http.Header{"X-Claude-Code-Session-Id": []string{id}},
		Metadata: map[string]any{},
	}
}

func TestSessionAffinityConcurrentColdBindIsAtomic(t *testing.T) {
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()
	auths := []*Auth{{ID: "a"}, {ID: "b"}}
	const requests = 32
	start := make(chan struct{})
	results := make(chan string, requests)
	errors := make(chan error, requests)
	var workers sync.WaitGroup
	for i := 0; i < requests; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			picked, err := selector.Pick(context.Background(), "claude", "model", explicitSessionOptions("same-session"), auths)
			if err != nil {
				errors <- err
				return
			}
			results <- picked.ID
		}()
	}
	close(start)
	workers.Wait()
	close(errors)
	close(results)
	for err := range errors {
		t.Fatalf("concurrent Pick() error = %v", err)
	}
	for id := range results {
		if id != "a" {
			t.Fatalf("concurrent session selected %q, want a", id)
		}
	}
	next, err := selector.Pick(context.Background(), "claude", "model", explicitSessionOptions("independent-session"), auths)
	if err != nil || next.ID != "b" {
		t.Fatalf("next independent session = %v, %v; want b", next, err)
	}
}

func TestSessionAffinityConcurrentLCPBindIsAtomic(t *testing.T) {
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()
	auths := []*Auth{{ID: "a"}, {ID: "b"}}
	const requests = 32
	start := make(chan struct{})
	results := make(chan string, requests)
	errors := make(chan error, requests)
	var workers sync.WaitGroup
	for i := 0; i < requests; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			opts := cliproxyexecutor.Options{
				SourceFormat:    sdktranslator.FormatOpenAI,
				OriginalRequest: []byte(`{"messages":[{"role":"user","content":"same prompt"}]}`),
				Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "same-caller"},
			}
			picked, err := selector.Pick(context.Background(), "openai", "model", opts, auths)
			if err != nil {
				errors <- err
				return
			}
			results <- picked.ID
		}()
	}
	close(start)
	workers.Wait()
	close(errors)
	close(results)
	for err := range errors {
		t.Fatalf("concurrent LCP Pick() error = %v", err)
	}
	for id := range results {
		if id != "a" {
			t.Fatalf("concurrent LCP selected %q, want a", id)
		}
	}
}

func TestSessionAffinityLCPWeakForkRebindsAndStaysSticky(t *testing.T) {
	selector := NewSessionAffinitySelector(&ExpiringFirstSelector{})
	defer selector.Stop()
	parentAuth := claudeQuotaAuth(t, "a", "0.90", "0.90", time.Now().Add(48*time.Hour), "allowed")
	otherAuth := claudeQuotaAuth(t, "b", "0.10", "0.10", time.Now().Add(48*time.Hour), "allowed")
	options := func(payload string) cliproxyexecutor.Options {
		return cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatOpenAI,
			OriginalRequest: []byte(payload),
			Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "fork-caller"},
		}
	}
	parentPayload := `{"messages":[{"role":"user","content":"start"},{"role":"assistant","content":"shared"},{"role":"user","content":"parent path"},{"role":"assistant","content":"parent answer"}]}`
	childPayload := `{"messages":[{"role":"user","content":"start"},{"role":"assistant","content":"shared"},{"role":"user","content":"child path"}]}`
	continuedPayload := `{"messages":[{"role":"user","content":"start"},{"role":"assistant","content":"shared"},{"role":"user","content":"child path"},{"role":"assistant","content":"child answer"},{"role":"user","content":"continue child"}]}`
	parent, err := selector.Pick(context.Background(), "claude", "model", options(parentPayload), []*Auth{parentAuth})
	if err != nil || parent.ID != "a" {
		t.Fatalf("parent = %v, %v; want a", parent, err)
	}
	for _, payload := range []string{childPayload, childPayload, continuedPayload} {
		child, errChild := selector.Pick(context.Background(), "claude", "model", options(payload), []*Auth{parentAuth, otherAuth})
		if errChild != nil || child.ID != "b" {
			t.Fatalf("child = %v, %v; want b", child, errChild)
		}
	}
	if counts := selector.matcher.ActiveSessionCounts(30 * time.Minute); counts["a"] != 1 || counts["b"] != 1 {
		t.Fatalf("LCP fork counts = %v, want one parent and one child", counts)
	}
	parentAgain, err := selector.Pick(context.Background(), "claude", "model", options(parentPayload), []*Auth{parentAuth, otherAuth})
	if err != nil || parentAgain.ID != "a" {
		t.Fatalf("parent after child = %v, %v; want a", parentAgain, err)
	}
}

func TestSessionAffinityExpiringFirstBalancesIndependentSessions(t *testing.T) {
	selector := NewSessionAffinitySelector(&ExpiringFirstSelector{})
	defer selector.Stop()
	reset := time.Now().Add(48 * time.Hour)
	auths := []*Auth{
		claudeQuotaAuth(t, "a", "0.10", "0.10", reset, "allowed"),
		claudeQuotaAuth(t, "b", "0.10", "0.10", reset, "allowed"),
	}
	auths[0].Attributes = map[string]string{AttributeAuthKind: AuthKindOAuth}
	auths[1].Attributes = map[string]string{AttributeAuthKind: AuthKindOAuth}
	auths[1].Quota = auths[0].Quota
	counts := map[string]int{}
	for i := 0; i < 6; i++ {
		picked, err := selector.Pick(context.Background(), "claude", "model", explicitSessionOptions(fmt.Sprintf("session-%d", i)), auths)
		if err != nil || picked == nil {
			t.Fatalf("session %d: Pick() = %v, %v", i, picked, err)
		}
		counts[picked.ID]++
	}
	if counts["a"] != 3 || counts["b"] != 3 {
		t.Fatalf("six equal sessions = %v, want 3/3", counts)
	}
}

func TestSessionAffinityExpiringFirstAdaptiveChild(t *testing.T) {
	for _, test := range []struct {
		name        string
		used        string
		unknown     bool
		wantChildID string
	}{
		{name: "healthy parent", used: "0.40", wantChildID: "a"},
		{name: "low headroom", used: "0.90", wantChildID: "b"},
		{name: "unknown headroom", unknown: true, wantChildID: "b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			selector := NewSessionAffinitySelector(&ExpiringFirstSelector{})
			defer selector.Stop()
			reset := time.Now().Add(48 * time.Hour)
			var parent *Auth
			if test.unknown {
				parent = &Auth{ID: "a", Provider: "claude", Status: StatusActive}
			} else {
				parent = claudeQuotaAuth(t, "a", test.used, test.used, reset, "allowed")
			}
			other := claudeQuotaAuth(t, "b", "0.10", "0.10", reset, "allowed")
			root := explicitSessionOptions("root")
			picked, err := selector.Pick(context.Background(), "claude", "model", root, []*Auth{parent})
			if err != nil || picked == nil || picked.ID != "a" {
				t.Fatalf("parent = %v, %v; want a", picked, err)
			}
			child := cliproxyexecutor.Options{
				Headers: http.Header{
					"X-Claude-Code-Session-Id": []string{"root"},
					"X-Claude-Code-Agent-Id":   []string{"child"},
				},
				Metadata: map[string]any{},
			}
			for i := 0; i < 2; i++ {
				selected, errChild := selector.Pick(context.Background(), "claude", "model", child, []*Auth{parent, other})
				if errChild != nil || selected == nil || selected.ID != test.wantChildID {
					t.Fatalf("child turn %d = %v, %v; want %s", i, selected, errChild, test.wantChildID)
				}
			}
			parentAgain, err := selector.Pick(context.Background(), "claude", "model", explicitSessionOptions("root"), []*Auth{parent, other})
			if err != nil || parentAgain == nil || parentAgain.ID != "a" {
				t.Fatalf("parent after child = %v, %v; want a", parentAgain, err)
			}
		})
	}
}

func TestSessionAffinityExpiringFirstKnownExhaustedPinFailsOver(t *testing.T) {
	for _, test := range []struct {
		name     string
		used5h   string
		usedWeek string
	}{
		{name: "five hour", used5h: "1.0", usedWeek: "0.10"},
		{name: "weekly", used5h: "0.10", usedWeek: "1.0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			selector := NewSessionAffinitySelector(&ExpiringFirstSelector{})
			defer selector.Stop()
			reset := time.Now().Add(48 * time.Hour)
			parent := claudeQuotaAuth(t, "a", "0.10", "0.10", reset, "allowed")
			other := claudeQuotaAuth(t, "b", "0.10", "0.10", reset, "allowed")
			options := explicitSessionOptions("held-session")
			first, err := selector.Pick(context.Background(), "claude", "model", options, []*Auth{parent})
			if err != nil || first == nil || first.ID != "a" {
				t.Fatalf("initial Pick() = %v, %v; want a", first, err)
			}
			parent.Quota = claudeQuotaAuth(t, "a", test.used5h, test.usedWeek, reset, "allowed").Quota
			selected, err := selector.Pick(context.Background(), "claude", "model", explicitSessionOptions("held-session"), []*Auth{parent, other})
			if err != nil || selected == nil || selected.ID != "b" {
				t.Fatalf("known exhausted pin = %v, %v; want b", selected, err)
			}
			parent.Quota = claudeQuotaAuth(t, "a", "0.10", "0.10", reset, "allowed").Quota
			selected, err = selector.Pick(context.Background(), "claude", "model", explicitSessionOptions("held-session"), []*Auth{parent, other})
			if err != nil || selected == nil || selected.ID != "b" {
				t.Fatalf("recovered former pin = %v, %v; want sticky b", selected, err)
			}
		})
	}
}

func TestSessionAffinityExpiringFirstLowNonzeroPinStaysSticky(t *testing.T) {
	selector := NewSessionAffinitySelector(&ExpiringFirstSelector{})
	defer selector.Stop()
	reset := time.Now().Add(48 * time.Hour)
	weak := claudeQuotaAuth(t, "a", "0.98", "0.98", reset, "allowed")
	strong := claudeQuotaAuth(t, "b", "0.10", "0.10", reset, "allowed")
	first, err := selector.Pick(context.Background(), "claude", "model", explicitSessionOptions("low-but-live"), []*Auth{weak})
	if err != nil || first == nil || first.ID != "a" {
		t.Fatalf("initial Pick() = %v, %v; want a", first, err)
	}
	selected, err := selector.Pick(context.Background(), "claude", "model", explicitSessionOptions("low-but-live"), []*Auth{weak, strong})
	if err != nil || selected == nil || selected.ID != "a" {
		t.Fatalf("live low pin = %v, %v; want a", selected, err)
	}
}

func TestTruncateSessionIDUsesStableFingerprint(t *testing.T) {
	first := "shared-prefix-aaaaaaaaaaaaaaaa"
	second := "shared-prefix-bbbbbbbbbbbbbbbb"
	if truncateSessionID(first) == truncateSessionID(second) || truncateSessionID(first) != truncateSessionID(first) {
		t.Fatalf("session fingerprints are not stable and distinct: %q, %q", truncateSessionID(first), truncateSessionID(second))
	}
	if got := truncateSessionID(first); len(got) != 12 || got == first[:12] {
		t.Fatalf("fingerprint %q is not a twelve-character digest", got)
	}
}

func TestSessionAffinityDefaultIdleTTL(t *testing.T) {
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()
	if selector.cache.ttl != 8*time.Hour || selector.matcher == nil {
		t.Fatalf("default affinity TTL = %s, want eight hours", selector.cache.ttl)
	}
	for i := 0; i < 2; i++ {
		picked, err := selector.Pick(context.Background(), "claude", "model", explicitSessionOptions(fmt.Sprintf("session-%d", i)), []*Auth{{ID: "a"}, {ID: "b"}})
		if err != nil || picked == nil {
			t.Fatalf("Pick() = %v, %v", picked, err)
		}
	}
}
