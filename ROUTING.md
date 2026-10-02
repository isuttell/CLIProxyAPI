# Subscription routing

Use `routing.strategy: expiring-first` with `routing.session-affinity: true`
to place new work using included subscription allowance while preserving active
conversations. Session affinity has an eight-hour idle TTL by default. It refreshes
on access, but bindings are in memory and do not survive a server restart.

Existing healthy bindings take precedence over allocation scores. New child
sessions and forks inherit their parent's account only when its observed allowance
can support more work. Otherwise they receive an independent binding. A hard
credential or quota failure permits failover; recovery of another account does
not move the session back.

Allocation considers the tightest live quota window, the urgency of unused weekly
allowance, and recently assigned sessions. It reserves a share of headroom for
each recently active session to avoid assigning a burst of new agents to one
account. These reservations are routing heuristics, not token balances or
provider-enforced limits.

The policy considers observations fresh for 30 minutes and reserves ten percentage
points per session active in the last 30 minutes. Fresh subscriptions with at least
15% headroom after reservations rank ahead of uncertain subscriptions, which rank
ahead of fresh subscriptions below that floor. Child inheritance requires at least
20% headroom after reserving for the prospective child. Weekly urgency adds up to
0.5 to the allocation score within a tier. These constants live together in
`sdk/cliproxy/auth/quota_standing.go`.

Unknown, elapsed, malformed, and stale quota observations remain explicitly
uncertain. They are not interpreted as an unused subscription. Quota is learned
from ordinary upstream responses; the router does not generate polling requests
or consume tokens to probe accounts. Claude and Codex supply the supported quota
windows. Other providers can still use session affinity and assigned-session load.

Included OAuth credentials are preferred to API-key credentials within the
eligible priority tier. Explicit credential priorities remain operator overrides.
An API key is a metered fallback, so omitting it prevents that fallback. Changing
weights affects `weighted-round-robin`; `expiring-first` uses quota observations
and assigned load instead.

Invalid routing strategies or affinity TTLs fail configuration loading and service
construction. An invalid hot reload preserves the active configuration and pins.
Changing valid routing settings replaces the selector and clears its bindings.

Run `go test ./test -run TestSubscriptionRoutingHTTP -count=1 -v` for the HTTP
acceptance harness. It exercises both streaming and ordinary Responses requests
through the handler and real Codex executor against a local upstream fixture,
checking sticky parent and child sessions, quota failover, and metered fallback.
It uses synthetic credentials and consumes no subscription allowance.
