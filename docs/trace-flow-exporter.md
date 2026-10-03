# Trace Flow exporter

CLIProxyAPI can export metadata for each completed upstream execution to Trace Flow Requests and Usage. The existing GUI usage feed continues independently. Trace Flow estimates API-equivalent cost centrally; the exporter sends no price, prompt, response, credential value, raw account identity, or failure body.

The exporter uses the [imported execution v2 contract](https://github.com/zaks-io/trace-flow/blob/main/docs/guides/imported-executions.md), official [OTLP protobuf types](https://pkg.go.dev/go.opentelemetry.io/proto/otlp/collector/trace/v1), and [bbolt transactions](https://pkg.go.dev/go.etcd.io/bbolt).

## Configuration

Export is disabled by default. In a v8 configuration:

```yaml
observability:
  trace-flow:
    enabled: true
    endpoint: "https://your-trace-flow-intake.example/v1/traces"
    outbox-path: "./state/trace-flow/outbox.db"
    api-key-env: "TRACE_FLOW_API_KEY"
    max-pending-bytes: 268435456
    min-free-bytes: 1073741824
```

Legacy configurations use the same `trace-flow` block at the root. Load the API key in the named environment variable, or the working directory's `.env` loaded by the server. Put only the variable name in YAML. Enabled startup fails if the key, destination, outbox, or installation state is invalid. The endpoint must be the full `/v1/traces` URL. Remote destinations require HTTPS, and redirects are refused.

Relative outbox paths resolve from the config directory. Keep state outside `auth-dir` so credential watchers and storage synchronization never transport the installation secret. Use a dedicated state directory at mode 0700, with the database and lock at 0600. On Windows, protected ACLs grant access only to the current user, SYSTEM, and Administrators. Existing state files are checked too. Startup refuses public state rather than changing its permissions. Back up the database as private installation state. Losing it changes installation identity and account groupings.

Hot reload can disable and re-enable an exporter that started enabled. Destination, state location, key variable, and storage limits require restart. Enabling after disabled startup also requires restart. Invalid reloads preserve the previous configuration. Changing a key value takes effect at restart and creates a new destination binding. Home config pushes preserve this local exporter configuration.

## What gets captured

Each upstream attempt keeps its execution UUID, including retries. Tokens use canonical schema v2 with uncached, cache read/write, non-reasoning output, reasoning, and unclassified buckets. Missing usage remains unknown; an explicit reported zero remains zero. Time to first token requires a substantive token event. Reported model and service tier remain absent when the provider omits them, so pricing can remain unpriced.

Codex provider-account coverage requires both workspace and member claims from its stored OAuth-acquired ID token. Claude requires both organization and account IDs with Anthropic OAuth provenance. Otherwise complete credential evidence gives credential coverage; incomplete evidence gives unknown coverage. References are installation-scoped HMACs. Historical payloads and references never change after local commit. Plugin and arbitrary SDK metadata cannot promote itself into provider-account coverage.

Only recognized native executor families are exported. OpenAI-compatible providers use the fixed `openai-compatibility` family, with the configured provider key used only inside the private account HMAC input. Calls that bypass CLIProxyAPI are outside coverage. Agent analytics remains separate.

## Durable delivery and recovery

Capture uses a bounded memory buffer and an asynchronous writer. Committed protobuf payloads survive restart. A crash before commit can lose buffered observations; overflow, storage budget, and disk losses are counted. The payload budget includes pending and quarantined records. The free-space reserve can stop new commits without blocking model serving.

An execution is removed only after HTTP 200, the exact `X-Trace-Flow-Contract: cliproxyapi.execution/2` marker, `X-Trace-Flow-Recording: true`, and a valid JSON acknowledgement with no rejections or error. Lost acknowledgements replay identical bytes. A bounded tombstone set suppresses recent duplicates. Trace Flow owns durable cross-delivery deduplication.

Transport failures, HTTP 429, and server errors retry with jitter and `Retry-After`. Authentication, recording denial, incompatible acknowledgement, and ambiguous partial success pause the binding. Ambiguous partial success also quarantines the submitted records. Isolatable validation errors and oversized batches split with a bounded request budget; invalid individual records remain quarantined. There is no acknowledgement bypass.

The endpoint and API key value form a private destination binding. Old pending records stay held when the binding changes. Rebinding changes delivery metadata only; it never changes the committed source content. Startup compaction uses a second file, synchronization, atomic replacement, and an exclusive lock. Insufficient compaction headroom is reported without deleting the original state.

Shutdown joins admitted HTTP handlers, tracked stream producers, usage dispatch, and local commits before stopping upload. Interrupted joins return an error and keep capture alive while a background join completes. External plugin-owned producers must obey the plugin shutdown contract.

## Offline maintenance

Stop the server first. Maintenance fails immediately when its outbox is locked. These flags use the existing server command:

```sh
cli-proxy-api --config config.yaml --trace-flow-status
cli-proxy-api --config config.yaml --trace-flow-resume
cli-proxy-api --config config.yaml --trace-flow-requeue reason:usage_invalid
cli-proxy-api --config config.yaml --trace-flow-rebind OLD_BINDING --trace-flow-rebind-to NEW_BINDING
cli-proxy-api --config config.yaml --trace-flow-discard execution:EXECUTION_UUID --trace-flow-confirm-discard
```

Requeue and discard selectors are `execution:UUID`, `binding:ID`, or `reason:CODE`. Requeue preserves payloads and changes selected quarantined records to pending. Resume separately clears the current binding pause without changing quarantined records, and requires its configured key. Rebind requires the configured destination key and its computed binding ID. The operator attests that both bindings belong to the same Organization; Trace Flow provides no identity verification endpoint for this operation. Discard requires its explicit confirmation flag and records an audit/counter.

Status emits sanitized JSON with pending/quarantined counts and bytes, binding IDs, pause and quarantine reasons, oldest pending timestamp, last commit/acknowledgement, retry time, capture rejection reasons, and loss counters. Runtime health uses structured logs. No management API feature is added.

The health flag includes cumulative loss diagnostics: counted capture loss, rejection, conflicts, or disk errors keep it false across restarts. Inspect counters, pauses, retry state, and last acknowledgement to distinguish past loss from a current delivery problem. A recovered network retry clears its current error after a durable acknowledgement.

Held backlogs still require a metadata scan each upload poll. Large backlogs can increase CPU usage until explicitly rebound or discarded.

Non-HTTP SDK or Home executions started after shutdown seals the dispatcher cannot be exported; a dropped late record emits a usage warning. Finish these executions before shutting down the service.

## Done

Implementation is complete when the server builds, the repository suite and relevant race checks pass, local service tests prove GUI fanout and shutdown, shared identity/usage fixtures match, and real dev exports reconcile with stored execution/token totals. Real OAuth multi-member and cross-Organization proofs require suitable test profiles and Organization credentials. Production enablement requires separate approval.
