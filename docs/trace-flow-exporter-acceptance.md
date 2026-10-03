# Trace Flow exporter acceptance

Verified against the cloud-dev imported-execution v2 receiver on October 2, 2026. Trace Flow contract baseline: `815cab47dc414b6ca9afc2321f7a482119ab1d32`.

## Real dev evidence

A built CLIProxyAPI server used a private temporary configuration and the configured development credentials. A local forwarding receiver initially returned 503, then forwarded the unchanged exporter payload to the development `/v1/traces` endpoint after server restart.

| Check | Observed result |
| --- | --- |
| Real streamed Claude call | HTTP 200, completed stream, 16 client-visible tokens |
| Real streamed Codex executor call using OpenAI API credentials | HTTP 200, completed stream, 269 client-visible tokens |
| Offline status before restart | Two pending executions, no quarantine or capture rejections |
| Development intake | HTTP 200, `cliproxyapi.execution/2` contract marker, recording true |
| Reported Claude metadata | `claude-haiku-4-5-20251001`, tier `standard`, credential coverage |
| Reported Codex metadata | `gpt-4o-mini-2024-07-18`, tier `default`, credential coverage |
| Offline status after acknowledgement | Zero pending executions, two tombstones |
| Exported canonical tokens | 285, equal to independent client-visible usage |
| Dev request-fact readback | Two executions, 285 tokens |
| Exact replay | Marked acknowledgement; execution and token totals remained two and 285 |
| Estimated cost status | Both unpriced, zero assessed estimate contribution; no invented price |

Readback used the named development deployment's read credential in process. No secret values, raw account inputs, prompts, or response bodies are retained in this document. No production configuration or deployment was changed.

## Local evidence

Tests cover official account identity vectors and all five shared execution-fixture cases, explicit zero versus missing usage, provisional stream zeros, partial usage on failure, substantive TTFT, credential identity provenance, whitelist canaries, strict acknowledgements, bounded splitting, binding changes, replay/conflicts, offline maintenance, storage budgets, abrupt process restart, compaction/locking, independent bbolt metadata, shutdown interruption, WebSocket drainage, Home updates, and GUI fanout. The implementation PR records the final build, repository suite, race checks, and review results.

## Final repository verification

- Darwin server build, Windows amd64 server build, Windows test compilation, and targeted `go vet` passed.
- Aggregate race tests passed for the exporter, account identity, config, CLI, Claude auth, capture helpers, HTTP handlers, service, auth manager, and usage dispatcher. The full executor race suite also passed.
- Full `go test ./...` passed every package except the live `TestAdvertiserAndBrowser_Integration` in `internal/discovery`. It fails because this machine cannot join the available IPv6 multicast interfaces. The exact test reproduces on the untouched base commit. No test was bypassed.
- Windows ACL tests are configured in the PR workflow; runtime Windows verification depends on that job.
- Opus cross-review found no high or medium residuals. Held-backlog polling still scans delivery metadata and may consume CPU for large retained backlogs; representative concurrency and sustained-outage resource measurements remain outstanding.

## Done

Real exporter delivery, client/exporter/stored token reconciliation, durable restart, and exact replay are proven in cloud-dev. Production enablement remains disabled by default.

The broader TRA-305 acceptance still needs genuine OAuth test profiles for two accounts, two members in one Codex workspace, and token refresh, plus a second Organization and a Collector credential for live isolation proofs. The available API keys exercise credential coverage. They do not establish provider-account coverage or satisfy those OAuth proofs. Dev catalog rates are needed to verify positive monetary estimates for these exact reported models and tiers. A dev consumer restart was not part of this run.
