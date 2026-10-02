package config

import (
	"fmt"
	"strings"
	"time"
)

// ValidateRouting rejects explicit routing values that cannot be applied.
func (cfg *Config) ValidateRouting() error {
	if cfg == nil {
		return fmt.Errorf("routing: configuration is nil")
	}

	switch strings.ToLower(strings.TrimSpace(cfg.Routing.Strategy)) {
	case "", "round-robin", "roundrobin", "rr",
		"weighted-round-robin", "weightedroundrobin", "wrr",
		"fill-first", "fillfirst", "ff",
		"expiring-first", "expiringfirst", "ef":
	default:
		return fmt.Errorf("routing.strategy: unsupported value %q", cfg.Routing.Strategy)
	}

	if rawTTL := strings.TrimSpace(cfg.Routing.SessionAffinityTTL); rawTTL != "" {
		ttl, errParse := time.ParseDuration(rawTTL)
		if errParse != nil {
			return fmt.Errorf("routing.session-affinity-ttl: %w", errParse)
		}
		if ttl <= 0 {
			return fmt.Errorf("routing.session-affinity-ttl: must be positive")
		}
	}
	return nil
}
