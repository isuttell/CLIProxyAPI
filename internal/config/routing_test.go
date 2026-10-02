package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoutingValidationAtConfigEntryPoints(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"unknown strategy", "routing: {strategy: unknown}", "routing.strategy"},
		{"malformed TTL", "routing: {session-affinity-ttl: someday}", "routing.session-affinity-ttl"},
		{"zero TTL", "routing: {session-affinity-ttl: 0s}", "routing.session-affinity-ttl"},
		{"negative TTL", "routing: {session-affinity-ttl: -1h}", "routing.session-affinity-ttl"},
		{"sub-second TTL", "routing: {session-affinity-ttl: 500ms}", "routing.session-affinity-ttl"},
		{"TTL sequence", "routing: {session-affinity-ttl: [1h]}", "cannot unmarshal"},
		{"strategy sequence", "routing: {strategy: [round-robin]}", "cannot unmarshal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, errParse := ParseConfigBytes([]byte(tt.raw)); errParse == nil || !strings.Contains(errParse.Error(), tt.want) {
				t.Fatalf("ParseConfigBytes() error = %v, want %s", errParse, tt.want)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if errWrite := os.WriteFile(path, []byte(tt.raw), 0o600); errWrite != nil {
				t.Fatal(errWrite)
			}
			for _, optional := range []bool{false, true} {
				if _, errLoad := LoadConfigOptional(path, optional); errLoad == nil || !strings.Contains(errLoad.Error(), tt.want) {
					t.Fatalf("LoadConfigOptional(optional=%t) error = %v, want %s", optional, errLoad, tt.want)
				}
			}
		})
	}
}

func TestRoutingValidationAcceptsOmittedValuesAndAliases(t *testing.T) {
	for _, strategy := range []string{"", "round-robin", "roundrobin", "rr", "weighted-round-robin", "weightedroundrobin", "wrr", "fill-first", "fillfirst", "ff", "expiring-first", "expiringfirst", "ef"} {
		cfg := &Config{Routing: RoutingConfig{Strategy: strategy, SessionAffinityTTL: " "}}
		if errValidate := cfg.ValidateRouting(); errValidate != nil {
			t.Fatalf("ValidateRouting(strategy=%q) error = %v", strategy, errValidate)
		}
	}
	if errValidate := (&Config{Routing: RoutingConfig{SessionAffinityTTL: "1s"}}).ValidateRouting(); errValidate != nil {
		t.Fatalf("positive TTL rejected: %v", errValidate)
	}
}
