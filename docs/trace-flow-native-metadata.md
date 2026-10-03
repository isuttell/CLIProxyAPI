# Native client metadata in Trace Flow executions

The `cliproxyapi.execution/2` exporter captures optional native client identifiers at request ingress. Routing may replace `cliproxyapi.session.id`, so use `cliproxyapi.client.session.id` when matching a Claude Code or Codex session to a native agent transcript. A generic `Session-Id` header alone does not identify the client source.

| Attribute | Source |
| --- | --- |
| `cliproxyapi.client.source` | `claude` or `codex`, only with protocol-specific evidence and a valid native session ID |
| `cliproxyapi.client.session.id` | Claude Code session ID, or Codex thread ID when present, otherwise Codex session ID |
| `cliproxyapi.client.agent.id` | Claude Code agent ID when explicitly present and not `main` |
| `cliproxyapi.client.session.parent_id` | Codex explicit parent thread ID when different from the chosen thread |
| `cliproxyapi.client.session.origin_id` | Codex session ID when different from the chosen thread |
| `cliproxyapi.inbound.trace_id`, `cliproxyapi.inbound.span_id` | A single valid W3C `traceparent` header on an HTTP request |

The native IDs are copied verbatim, with a 128-character bound and a restricted token alphabet. Invalid or sensitive values are omitted with capture omission counters. Codex role names are not agent IDs. If a Codex child turn has no proven thread ID, its session identity is omitted when the request explicitly signals a subagent. A chosen thread ID equal to an explicit parent is also omitted as ambiguous.

The inbound trace context is data for searching and correlation. It does not change the execution span's trace ID, span ID, parent, links, or scope. The exporter uses the official [OpenTelemetry Go `TraceContext` propagator](https://pkg.go.dev/go.opentelemetry.io/otel/propagation#TraceContext.Extract) to parse a fresh carrier containing only `traceparent`; it does not install a global propagator or forward the extracted context. This follows the [W3C Trace Context specification](https://www.w3.org/TR/trace-context/#traceparent-header). WebSocket turns do not capture the upgrade request's inbound trace context.

Trace Flow must accept these keys before a proxy release emits them. The v2 validator has a strict attribute allowlist, and an unknown key rejects its span. The maximum remains 32 span attributes. Previous outbox entries retain their original protobuf bytes and replay unchanged.
