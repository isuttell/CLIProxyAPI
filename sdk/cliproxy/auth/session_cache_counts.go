package auth

import (
	"strings"
	"time"
)

// ActiveSessionCounts counts recently touched logical sessions by auth.
// Alias groups can be refreshed in any order, so every alias participates in deduplication.
func (c *SessionCache) ActiveSessionCounts(window time.Duration) map[string]int {
	counts := make(map[string]int)
	if c == nil || window <= 0 {
		return counts
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := c.now()
	parents := make(map[string]map[string]string)
	for _, entry := range c.groups {
		if entry.authID == "" || !now.Before(entry.expiresAt) || now.Sub(entry.expiresAt.Add(-c.ttl)) > window {
			continue
		}
		ids := parents[entry.authID]
		if ids == nil {
			ids = make(map[string]string)
			parents[entry.authID] = ids
		}
		first := ""
		for _, alias := range entry.aliases {
			id, ok := logicalSessionID(alias)
			if !ok {
				continue
			}
			if _, exists := ids[id]; !exists {
				ids[id] = id
			}
			if first == "" {
				first = id
			} else {
				ids[sessionCountRoot(ids, id)] = sessionCountRoot(ids, first)
			}
		}
	}
	for authID, ids := range parents {
		roots := make(map[string]struct{})
		for id := range ids {
			roots[sessionCountRoot(ids, id)] = struct{}{}
		}
		counts[authID] = len(roots)
	}
	return counts
}

func logicalSessionID(alias string) (string, bool) {
	_, sessionAndModel, ok := strings.Cut(alias, "::")
	if !ok {
		return "", false
	}
	modelSeparator := strings.LastIndex(sessionAndModel, "::")
	if modelSeparator <= 0 {
		return "", false
	}
	return sessionAndModel[:modelSeparator], true
}

func sessionCountRoot(parents map[string]string, id string) string {
	root := id
	for parents[root] != root {
		root = parents[root]
	}
	for id != root {
		next := parents[id]
		parents[id] = root
		id = next
	}
	return root
}
