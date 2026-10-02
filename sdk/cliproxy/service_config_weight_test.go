package cliproxy

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestWeightedRoundRobinRoutingSelector(t *testing.T) {
	state, errRouting := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "wrr"},
	})
	if errRouting != nil {
		t.Fatal(errRouting)
	}
	if state.strategy != "weighted-round-robin" {
		t.Fatalf("strategy = %q, want weighted-round-robin", state.strategy)
	}
	if _, ok := newRoutingSelector(state).(*coreauth.WeightedRoundRobinSelector); !ok {
		t.Fatalf("selector type = %T, want *auth.WeightedRoundRobinSelector", newRoutingSelector(state))
	}
}

func TestExpiringFirstRoutingSelector(t *testing.T) {
	state, errRouting := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "expiring-first"},
	})
	if errRouting != nil {
		t.Fatal(errRouting)
	}
	if state.strategy != "expiring-first" {
		t.Fatalf("strategy = %q, want expiring-first", state.strategy)
	}
	if _, ok := newRoutingSelector(state).(*coreauth.ExpiringFirstSelector); !ok {
		t.Fatalf("selector type = %T, want *auth.ExpiringFirstSelector", newRoutingSelector(state))
	}
}

func TestRoutingDefaultsToEightHourIdleAffinity(t *testing.T) {
	state, errRouting := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{SessionAffinity: true}})
	if errRouting != nil {
		t.Fatal(errRouting)
	}
	if state.sessionAffinityTTL != 8*time.Hour {
		t.Fatalf("session affinity TTL = %s, want 8h", state.sessionAffinityTTL)
	}
}

func TestBuilderRejectsInvalidRouting(t *testing.T) {
	for _, routing := range []internalconfig.RoutingConfig{
		{Strategy: "unknown"},
		{SessionAffinityTTL: "later"},
		{SessionAffinityTTL: "0s"},
		{SessionAffinityTTL: "500ms"},
	} {
		_, errBuild := NewBuilder().WithConfig(&internalconfig.Config{Routing: routing}).WithConfigPath(t.TempDir() + "/config.yaml").Build()
		if errBuild == nil {
			t.Fatalf("Build() accepted invalid routing %+v", routing)
		}
	}
}

func TestServiceRejectsInvalidRoutingConfigCommit(t *testing.T) {
	originalCfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "round-robin"}}
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	service := &Service{cfg: originalCfg, coreManager: manager}
	originalSelector := manager.Selector()
	for _, routing := range []internalconfig.RoutingConfig{
		{Strategy: "unknown"},
		{SessionAffinityTTL: "invalid"},
		{SessionAffinityTTL: "-1h"},
		{SessionAffinityTTL: "500ms"},
	} {
		if service.applyConfigUpdateWithAuthSynthesis(nil, &internalconfig.Config{Routing: routing}, true) {
			t.Fatalf("hot config accepted invalid routing %+v", routing)
		}
		if service.cfg != originalCfg || service.configSequence != 0 || manager.Selector() != originalSelector {
			t.Fatal("invalid hot config changed active config or selector")
		}
	}
}

func TestServiceRejectsInvalidCredentialWeightConfigCommit(t *testing.T) {
	originalCfg := &internalconfig.Config{}
	service := &Service{cfg: originalCfg}
	invalidWeight := internalconfig.MaxCredentialWeight + 1
	newCfg := &internalconfig.Config{
		VertexCompatAPIKey: []internalconfig.VertexCompatKey{{
			APIKey: "vertex-key",
			Weight: &invalidWeight,
		}},
	}

	if service.applyConfigUpdateWithAuthSynthesis(nil, newCfg, true) {
		t.Fatal("hot config application accepted an invalid credential weight")
	}
	if service.cfg != originalCfg {
		t.Fatal("invalid hot config replaced the active config")
	}
	if service.configSequence != 0 {
		t.Fatalf("config sequence = %d, want 0", service.configSequence)
	}
}

type trackingStoppableSelector struct {
	stopped bool
}

func (s *trackingStoppableSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*coreauth.Auth) (*coreauth.Auth, error) {
	return nil, nil
}

func (s *trackingStoppableSelector) Stop() {
	s.stopped = true
}

func TestApplyManagerConfigStopsReplacedServiceAffinitySelector(t *testing.T) {
	tracking := &trackingStoppableSelector{}
	service := &Service{
		coreManager: coreauth.NewManager(nil, tracking, nil),
	}

	newCfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy: "round-robin",
		},
	}
	commit := configCommit{cfg: newCfg, sequence: 1}
	if !service.applyManagerConfig(context.Background(), commit) {
		t.Fatal("applyManagerConfig failed")
	}

	if !tracking.stopped {
		t.Fatal("expected replaced selector to be stopped during routing config apply")
	}
}
