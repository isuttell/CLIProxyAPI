package auth

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func registerQuotaRoutingAuth(t *testing.T, manager *Manager, auth *Auth, models ...string) {
	t.Helper()
	modelInfos := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		modelInfos = append(modelInfos, &registry.ModelInfo{ID: model})
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, modelInfos)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register(%s): %v", auth.ID, err)
	}
}

func TestManagerPassiveExhaustionFallsThroughPriority(t *testing.T) {
	for _, tc := range []struct {
		name        string
		affinity    bool
		mixed       bool
		keyFallback bool
	}{
		{"single", false, false, false},
		{"mixed", false, true, false},
		{"affinity", true, false, false},
		{"metered fallback", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := "quota-priority-model"
			high := claudeQuotaAuth(t, "quota-high-"+tc.name, "1", ".2", time.Now().Add(24*time.Hour), "allowed")
			high.Attributes = map[string]string{"priority": "10", AttributeAuthKind: AuthKindOAuth}
			low := claudeQuotaAuth(t, "quota-low-"+tc.name, ".2", ".2", time.Now().Add(24*time.Hour), "allowed")
			low.Attributes = map[string]string{"priority": "0", AttributeAuthKind: AuthKindOAuth}
			if tc.keyFallback {
				low.Attributes[AttributeAuthKind] = AuthKindAPIKey
			}
			var selector Selector = &ExpiringFirstSelector{}
			if tc.affinity {
				wrapped := NewSessionAffinitySelector(selector)
				defer wrapped.Stop()
				selector = wrapped
			}
			manager := NewManager(nil, selector, nil)
			manager.RegisterExecutor(&refreshMockExecutor{id: "claude"})
			registerQuotaRoutingAuth(t, manager, high, model)
			registerQuotaRoutingAuth(t, manager, low, model)
			var picked *Auth
			var err error
			if tc.mixed {
				picked, _, _, err = manager.pickNextMixed(context.Background(), []string{"claude"}, model, cliproxyexecutor.Options{}, nil)
			} else {
				picked, _, err = manager.pickNext(context.Background(), "claude", model, cliproxyexecutor.Options{}, nil)
			}
			if err != nil || picked == nil || picked.ID != low.ID {
				t.Fatalf("pickNext() = %v, %v; want %s", picked, err, low.ID)
			}
		})
	}
}

func TestManagerPassiveExhaustionReportsLastRequiredReset(t *testing.T) {
	now := time.Now()
	auth := claudeQuotaAuth(t, "quota-both-exhausted", "1", "1", now.Add(2*time.Hour), "allowed")
	auth.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
	manager := NewManager(nil, &ExpiringFirstSelector{}, nil)
	_, err := manager.availableAuthsForRouteModel([]*Auth{auth}, "claude", "model", now)
	var cooldown *modelCooldownError
	if !errors.As(err, &cooldown) || cooldown.resetIn < 2*time.Hour-time.Second || cooldown.resetIn > 2*time.Hour+time.Second {
		t.Fatalf("availability error = %v, want cooldown near 2h", err)
	}
}

func TestManagerPassiveExhaustionLeavesOtherSelectorsAndPluginAlone(t *testing.T) {
	now := time.Now()
	high := claudeQuotaAuth(t, "quota-high-unfiltered", "1", ".2", now.Add(24*time.Hour), "allowed")
	high.Attributes = map[string]string{"priority": "10"}
	low := claudeQuotaAuth(t, "quota-low-unfiltered", ".2", ".2", now.Add(24*time.Hour), "allowed")
	low.Attributes = map[string]string{"priority": "0"}
	for _, tc := range []struct {
		name     string
		selector Selector
		plugin   PluginScheduler
		want     string
	}{
		{"round robin", &RoundRobinSelector{}, nil, high.ID},
		{"active plugin", &ExpiringFirstSelector{}, &fakePluginScheduler{}, high.ID},
		{"inactive host", &ExpiringFirstSelector{}, &inactivePluginScheduler{}, low.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, tc.selector, nil)
			if tc.plugin != nil {
				manager.SetPluginScheduler(tc.plugin)
			}
			available, err := manager.availableAuthsForRouteModel([]*Auth{high, low}, "claude", "model", now)
			if err != nil || len(available) != 1 || available[0].ID != tc.want {
				t.Fatalf("available = %v, %v; want %s", available, err, tc.want)
			}
		})
	}
}

func TestManagerAliasSpecificCodexQuotaChangesPlacement(t *testing.T) {
	const routeModel = "public-codex-spark"
	const targetModel = "gpt-5.3-codex-spark"
	manager := NewManager(nil, &ExpiringFirstSelector{}, nil)
	manager.RegisterExecutor(&refreshMockExecutor{id: "codex"})
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"codex": {{Name: targetModel, Alias: routeModel, Fork: true}},
	})
	limited := codexQuotaAuth(t, "a-limited-spark", "10", time.Now().Add(24*time.Hour))
	limited.Attributes = map[string]string{AttributeAuthKind: AuthKindOAuth}
	prefix := "X-Codex-Additional-Gpt-5.3-Codex-Spark-"
	limited.Quota.Signals[prefix+"Limit-Name"] = targetModel
	limited.Quota.Signals[prefix+"Primary-Used-Percent"] = "98"
	limited.Quota.Signals[prefix+"Primary-Window-Minutes"] = "300"
	limited.Quota.Signals[prefix+"Primary-Reset-At"] = strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	healthy := codexQuotaAuth(t, "b-healthy-spark", "10", time.Now().Add(24*time.Hour))
	healthy.Attributes = map[string]string{AttributeAuthKind: AuthKindOAuth}
	for _, auth := range []*Auth{limited, healthy} {
		registerQuotaRoutingAuth(t, manager, auth, routeModel, targetModel)
	}
	picked, _, err := manager.pickNext(context.Background(), "codex", routeModel, cliproxyexecutor.Options{}, nil)
	if err != nil || picked == nil || picked.ID != healthy.ID {
		t.Fatalf("pickNext(alias) = %v, %v; want %s", picked, err, healthy.ID)
	}
	if got := selectionArgForSelector(&ExpiringFirstSelector{}, routeModel); got != routeModel {
		t.Fatalf("selection argument = %q, want route model", got)
	}
}

func TestManagerModelSpecificCodexExhaustionCannotDispatch(t *testing.T) {
	const model = "gpt-5.3-codex-spark"
	manager := NewManager(nil, &ExpiringFirstSelector{}, nil)
	manager.RegisterExecutor(&refreshMockExecutor{id: "codex"})
	auth := codexQuotaAuth(t, "codex-spark-exhausted", "10", time.Now().Add(24*time.Hour))
	auth.Attributes = map[string]string{AttributeAuthKind: AuthKindOAuth}
	prefix := "X-Codex-Additional-Gpt-5.3-Codex-Spark-"
	auth.Quota.Signals[prefix+"Limit-Name"] = model
	auth.Quota.Signals[prefix+"Primary-Used-Percent"] = "100"
	auth.Quota.Signals[prefix+"Primary-Window-Minutes"] = "300"
	auth.Quota.Signals[prefix+"Primary-Reset-At"] = strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	registerQuotaRoutingAuth(t, manager, auth, model)
	_, _, err := manager.pickNext(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil)
	var cooldown *modelCooldownError
	if !errors.As(err, &cooldown) || cooldown.resetIn <= 0 {
		t.Fatalf("pickNext() = %v, want model cooldown", err)
	}
}
