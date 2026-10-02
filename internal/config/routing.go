package config

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const DefaultSessionAffinityTTL = 8 * time.Hour

// ValidateRouting rejects explicit routing values that cannot be applied.
func (cfg *Config) ValidateRouting() error {
	if cfg == nil {
		return fmt.Errorf("routing: configuration is nil")
	}
	_, _, errNormalize := cfg.Routing.Normalize()
	return errNormalize
}

// Normalize resolves supported strategy aliases and the effective affinity TTL.
func (routing RoutingConfig) Normalize() (string, time.Duration, error) {
	var strategy string
	switch strings.ToLower(strings.TrimSpace(routing.Strategy)) {
	case "", "round-robin", "roundrobin", "rr":
		strategy = "round-robin"
	case "weighted-round-robin", "weightedroundrobin", "wrr":
		strategy = "weighted-round-robin"
	case "fill-first", "fillfirst", "ff":
		strategy = "fill-first"
	case "expiring-first", "expiringfirst", "ef":
		strategy = "expiring-first"
	default:
		return "", 0, fmt.Errorf("routing.strategy: unsupported value %q", routing.Strategy)
	}

	ttl := DefaultSessionAffinityTTL
	if rawTTL := strings.TrimSpace(routing.SessionAffinityTTL); rawTTL != "" {
		parsedTTL, errParse := time.ParseDuration(rawTTL)
		if errParse != nil {
			return "", 0, fmt.Errorf("routing.session-affinity-ttl: %w", errParse)
		}
		if parsedTTL < time.Second {
			return "", 0, fmt.Errorf("routing.session-affinity-ttl: must be at least 1s")
		}
		ttl = parsedTTL
	}
	return strategy, ttl, nil
}

// routingDecodeError distinguishes malformed routing fields from unrelated
// optional config decode errors, which retain the existing empty-config fallback.
func routingDecodeError(data []byte) error {
	var doc yaml.Node
	if errParse := yaml.Unmarshal(data, &doc); errParse != nil || len(doc.Content) == 0 {
		return nil
	}
	routingNode := yamlPath(doc.Content[0], "routing")
	if routingNode == nil {
		return nil
	}
	var routing RoutingConfig
	if errDecode := routingNode.Decode(&routing); errDecode != nil {
		return fmt.Errorf("routing: %w", errDecode)
	}
	return nil
}
