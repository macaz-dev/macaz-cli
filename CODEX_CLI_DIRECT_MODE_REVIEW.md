# Codex-CLI direct-mode fix

This document is intended for an independent implementation review. It records
what changed, why it changed, the evidence used, and the remaining limitations.

## Outcome

The Codex-CLI provider still uses `codex app-server` for the user's existing
Codex installation and authentication, but it no longer lets Codex's model
catalog force Claude's tools through Codex Code Mode.

Claude Code remains the orchestrator and executor:

1. Claude sends its tool schemas to macaz.
2. Macaz exposes them to app-server as client-owned `dynamicTools`.
3. The upstream model returns normal direct function calls.
4. Claude executes those tools.
5. Macaz returns the structured tool results to the pending app-server JSON-RPC
   requests on the same thread.

Codex does not receive an execution environment and cannot edit the project,
run a shell, browse, start apps, load skills, or spawn subagents on behalf of
Claude.

## Root cause

`gpt-5.6-sol` and the other current GPT-5.6 Codex catalog entries advertise
`tool_mode: "code_mode_only"`. Codex core gives this server-provided selector
precedence over local `--disable code_mode` feature flags.

As a result, app-server hid the client dynamic tools behind its own JavaScript
`exec` tool. A request for two Claude tools became approximately:

```js
await Promise.all([
  tools.LookupAlpha(...),
  tools.LookupBeta(...),
]);
```

Client dynamic tool callbacks do not become available to macaz as one atomic
batch through this nested path. App-server waits for a callback result before
it can expose later calls. A timer in macaz therefore could not repair the
serialization: increasing the grace interval from 300 ms to 1.5 s only delayed
the first visible call.

This explained the observed symptoms:

- Claude appeared idle while the nested Codex agent reasoned.
- tool calls arrived serially even when the nested code used `Promise.all`;
- Codex could attempt its own file edits before Claude received a tool call;
- duplicate orchestration increased latency and token use;
- interrupt/restart fallbacks caused replanning and occasional loops.

The official app-server documentation also describes the surface as a Codex
agent integration with threads, turns, approvals, and agent events. It does not
document a raw Responses passthrough or a model-only mode:
<https://learn.chatgpt.com/docs/app-server>.

## Implementation

### 1. Override only runtime selectors in a temporary model catalog

`internal/provider/codexcli/pool.go` reads model metadata from the local Codex
`models_cache.json`, copies only the `models` array into the app-server's private
temporary directory, and changes:

```json
{
  "tool_mode": "direct",
  "multi_agent_version": null
}
```

All other live model metadata is preserved, including model IDs, context
limits, supported effort levels, modalities, and backend compatibility fields.
The user's source catalog is opened read-only and is never modified. The
temporary catalog is written with mode `0600` and removed with the app-server
directory.

Codex 0.144.x still requires the legacy boolean
`supports_reasoning_summaries`. Newer remote catalog payloads can omit it (newer
Codex defaults the replacement capability to `true`) or advertise
`supports_reasoning_summary_parameter`. Macaz preserves an existing legacy
value, maps the replacement boolean when present, and otherwise backfills the
new-client default of `true`. This prevents a fresh remote cache refresh from
making the installed CLI unable to parse its own catalog.

Codex refreshes `models_cache.json` asynchronously. The provider therefore
loads and validates the direct catalog once, on the first pool startup, and
pins that immutable byte snapshot for every later app-server in the same
provider. Claude Auto Mode can start a second concurrent app-server for its
permission classifier, so every server in the pool must observe the same
validated metadata revision.

The manual startup failure was separate from that in-process race: the first
app-server received a freshly refreshed catalog without the legacy reasoning
field and exited before `initialize`. The compatibility normalization above is
the fix for that failure; snapshot pinning prevents later pool members from
drifting after initialization.

The pin is scoped to one provider lifetime. Restarting macaz intentionally
loads current metadata again. If the initial snapshot is incompatible with the
installed Codex version, startup fails explicitly and a later retry may load a
completed cache refresh; an already validated pool never changes schema under
active requests.

The override is passed using Codex's `model_catalog_json` configuration. If no
valid local model cache exists, the provider fails clearly instead of silently
falling back to the broken Code Mode behavior.

Catalog discovery is profile-agnostic. Macaz considers the default profile,
all local sibling `.codex*` profiles, an inherited `CODEX_HOME`, and an absolute
`CODEX_HOME` reported by a configured wrapper's version output. The wrapper
profile has highest merge precedence because it is the environment that the
actual app-server receives. A malformed cache is skipped when another profile
is valid. Completely custom layouts can set `MACAZ_CODEX_MODEL_CATALOG` to one
explicit catalog; that override is strict so configuration mistakes are not
silently masked.

### 2. Remove Codex-owned execution surfaces

`thread/start` now uses:

```json
{
  "sandbox": "read-only",
  "environments": []
}
```

An empty `environments` array is the app-server protocol's explicit way to
disable execution environments. This removes shell, patch, and image-view
runtime tools. Existing feature/config overrides also disable browser, apps,
web search, image generation, MCP servers, plugins, skills, tool suggestions,
request-user-input, collaboration context, and multi-agent features.

A short developer instruction tells the upstream that it is an inference
backend for Claude and may call only client-provided tools. Any unexpected
native app-server item, including the remaining built-in plan utility, is
rejected fail-closed.

### 3. Preserve structured same-thread continuation

The existing pending JSON-RPC mechanism remains. Tool turns are parked by the
Claude session/agent cache key, Claude returns matching tool results, and macaz
responds to the pending app-server requests without replaying the whole task.

Parallel direct callbacks are collected into one Claude response when the
model emits them. The 300 ms quiet interval remains only as a batch boundary;
it no longer attempts to compensate for nested Code Mode serialization.

The model may still choose a sequential tool plan. Macaz preserves that choice
instead of forcing parallelism.

### 4. Stream visible reasoning progress

Non-compaction turns with reasoning enabled now request `summary: "auto"`.
`item/reasoning/summaryTextDelta` events are translated to Claude-compatible
`thinking` blocks with a synthetic macaz signature. Compaction requests do not
request summaries, avoiding unnecessary output tokens.

The upstream model decides whether to emit a summary, so a short request can
still have a few seconds without visible progress.

## Reference-project comparison

`claude-code-proxy` sends Anthropic requests directly to
`https://chatgpt.com/backend-api/codex/responses`, translates Responses tool
calls, and owns a separate OAuth login. It does not use native Codex CLI
credentials or app-server.

Macaz's `OpenAI Subscription` provider already follows that direct architecture
and remains the cleaner option when a user connects OpenAI OAuth through macaz.
The Codex-CLI provider exists for users who explicitly want to reuse the local
Codex installation. The temporary catalog makes app-server approximate a
direct inference transport without reading or exporting its authentication
tokens.

Ollama's Claude integration similarly exposes an Anthropic-compatible HTTP
surface and does not insert a second coding agent between Claude and the model.

## Verification

Automated coverage includes:

- model-catalog copying, selector overrides, and old/new reasoning-capability
  schema normalization;
- proof that the source catalog remains unchanged;
- a cache-refresh regression test proving that later app-servers reuse the
  first validated catalog even if the source is replaced with incomplete JSON;
- read-only/no-environment thread parameters;
- reasoning-summary translation;
- parallel direct dynamic tool batching;
- same-thread tool-result continuation;
- queued-message fallback, expiry, concurrency, and pool reuse;
- model discovery, attachments, context overflow, and compaction effort.

Live tests used `codex-cli 0.144.6` with `gpt-5.6-sol`:

- the unnormalized current cache failed deterministically with
  `missing field supports_reasoning_summaries`;
- after normalization, four runs each started four concurrent app-servers via
  the configured `wcodex` wrapper with no parent `CODEX_HOME` (16 startups);
- complete tool call + tool result + final response passed on one thread in
  8.47 seconds through `wcodex`;
- a direct tool call appeared in 6.41 seconds through `wcodex`;
- app-server logged `ToolCall: LookupAlpha ...`, not `ToolCall: exec ...`;
- the direct call ID used the upstream `call_...` form, not the nested
  `exec-...` form.

These timings are single diagnostic samples, not performance benchmarks.

Project validation commands:

```text
go test ./...
go vet ./...
go test -race ./...
```

## Review questions and remaining limitations

1. The bridge relies on Codex's local `models_cache.json` schema and the
   `model_catalog_json` config supported by Codex 0.144.6. Unknown metadata is
   preserved opaquely, but a breaking schema or selector change must fail
   explicitly rather than restore Code Mode silently.
2. App-server remains an agent protocol internally. The execution surfaces are
   removed and native items are rejected, but this is not identical to a raw
   Responses HTTP connection.
3. Reasoning summaries are best-effort. Codex can omit them for simple prompts.
4. The model controls whether independent tool calls are parallel or
   sequential. Macaz batches genuine parallel callbacks but does not fabricate
   them.
5. The direct `OpenAI Subscription` provider remains preferable when reusing
   Codex CLI authentication is not a requirement.

Reviewers should pay particular attention to catalog compatibility across
Codex releases, JSON-RPC event ordering for parallel dynamic tools, and whether
any newly introduced app-server item type could bypass the fail-closed native
tool check.
