# macaz

[![CI](https://github.com/macaz-dev/macaz-cli/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/macaz-dev/macaz-cli/actions/workflows/ci.yml)

**Use your favorite models and providers with your favorite coding agents.**

macaz connects locally installed coding agents with a model provider chosen by
the user. It starts Claude Code or Codex CLI and routes model interactions while
the selected agent continues to own its local tools, permissions, sessions,
skills, and user interface.

macaz is free and open-source software licensed under Apache-2.0. Every build
contains the complete feature set.

> [!IMPORTANT]
> macaz is an independent interoperability project. It is not affiliated with,
> authorized by, endorsed by, or sponsored by Anthropic, OpenAI, or any other
> client or model provider. Claude Code and Codex CLI are separate products that
> must be obtained from their authorized sources. Each product and provider
> remains subject to its own current terms.

Read [LEGAL.md](LEGAL.md) and [PRIVACY.md](PRIVACY.md) before use.

## What it does

```text
macaz claude   # Claude Code through the selected provider
macaz codex    # Codex CLI through the selected provider
```

The two clients have independent provider/model configuration and isolated
macaz profiles. For example, `macaz claude` can use an OpenAI model while
`macaz codex` uses the official Anthropic API. `macaz reset codex` changes
neither the Claude configuration nor shared provider credentials.

The local routing layer translates both directions:

- Anthropic Messages requests and streams used by Claude Code;
- OpenAI Responses requests and streams used by Codex CLI;
- text, images, supported document inputs, reasoning effort, usage, errors,
  function tools, Codex custom/free-form tools, and tool namespaces; and
- live provider model catalogs into each client's native `/model` interface.

Client-executed tools stay in the client. Shell commands, file edits,
`apply_patch`, skills, hooks, MCP tools, namespaced tools, and subagents are not
executed by macaz or by an upstream CLI adapter. Normal client permission and
sandbox behavior is preserved unless the user explicitly passes that client's
own bypass option.

Server-executed tools are different: a tool such as provider-hosted web
search is available only when the selected provider exposes a compatible
server implementation. macaz disables Codex's OpenAI-hosted web-search request
for provider-neutral sessions rather than silently pretending another provider
can execute it. Local function, custom, namespace, and MCP tools remain
available.

No translation can make different proprietary models semantically identical.
The compatibility target is correct protocol and local-tool behavior without a
hidden provider fallback.

## Install

On macOS or Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/macaz-dev/macaz-cli/main/scripts/install.sh | sh
```

The installer selects the platform release, requires its entry in
`SHA256SUMS`, verifies SHA-256, and installs to `$HOME/.local/bin` by default.
Pin a release with `--version`:

```sh
curl -fsSL https://raw.githubusercontent.com/macaz-dev/macaz-cli/main/scripts/install.sh |
  sh -s -- --version 0.2.0
```

Windows binaries and other release files are published on
[GitHub Releases](https://github.com/macaz-dev/macaz-cli/releases).

Install the latest release in place later with:

```text
macaz update
```

The updater downloads only the exact asset for the current platform, verifies
the release checksums and embedded version, and replaces the executable with
rollback protection. It does not send prompts, source code, configuration, or
provider credentials.

Release builds perform a short best-effort update check on startup. The check
never installs automatically and does not block offline use. Disable it with:

```sh
export MACAZ_NO_UPDATE_CHECK=1
```

## Requirements

- Go is needed only when building from source.
- `claude` must be on `PATH` to use `macaz claude`.
- `codex` must be on `PATH` to use `macaz codex`.
- `opencode` is required only when OpenCode CLI is the selected provider.
- A browser is used by the OpenAI Subscription authorization flow.

Claude Code, Codex CLI, and OpenCode are not bundled or redistributed.

## Setup and providers

The first invocation of each client opens its own setup:

```text
macaz claude
macaz codex
```

Available upstreams:

| Provider | Claude client | Codex client |
| --- | --- | --- |
| OpenAI Subscription | yes | yes |
| OpenAI API | yes | yes |
| OpenRouter API | yes | yes |
| Anthropic API | yes | yes |
| Codex CLI provider bridge | yes | no (recursive) |
| OpenCode CLI provider bridge | yes | yes |

API keys and OAuth credentials are stored in the operating-system credential
store, not in `config.json`. The Anthropic option uses an Anthropic API key and
the public Messages API; it does not use or convert a Claude consumer
subscription.

Each start refreshes the active provider catalog. The resulting public model
IDs and supported reasoning levels are written to the isolated client profile,
so model selection works through Claude Code's or Codex CLI's native `/model`
interface. Model selection is saved by the client for that isolated profile.

## Commands

```text
macaz                     Start Claude Code (backward-compatible default)
macaz claude [args...]    Start Claude Code
macaz codex [args...]     Start Codex CLI
macaz status [client]     Check the configured provider and model catalog
macaz doctor [client]     Check the client executable and provider
macaz reset [client]      Reset one client, or all macaz state when omitted
macaz legal               Show the compatibility notice
macaz update              Install the latest verified release
macaz version             Show the installed version
```

Arguments after `macaz claude` or `macaz codex` are forwarded to that client,
except model/provider/profile overrides that would bypass macaz's authenticated
local routing layer.

Examples:

```sh
macaz claude --dangerously-skip-permissions
macaz codex --sandbox workspace-write
macaz status codex
macaz doctor claude
macaz reset codex
```

`macaz reset claude` or `macaz reset codex` removes only that client's macaz
configuration and isolated profile. It preserves the other client and shared
provider credentials. `macaz reset` with no client removes both macaz profiles,
all macaz-managed credentials, configuration, and isolated session history. It
does not remove normal vendor-client profiles or credentials managed directly
by vendor CLIs.

## Isolation and security

- The local routing layer listens on a random `127.0.0.1` port.
- A new random authentication token is generated for every launch.
- The token exists only in the child process environment.
- Config and profile files use private permissions where supported.
- Normal Claude and Codex profiles are not modified.
- The local routing process stops when the launched client exits.
- There is no fallback to a different provider or official model.
- macaz contains no project-operated analytics or telemetry client.

The isolated profiles may contain client-owned session history. See
[PRIVACY.md](PRIVACY.md) for exact data flow and deletion behavior.

## Provider notes

- OpenAI API and OpenRouter use Responses-compatible HTTP adapters.
- OpenAI Subscription uses device authorization, refreshes credentials, and
  applies bounded account retry/backoff. It is experimental and remains
  subject to OpenAI's current account terms.
- Anthropic API uses native `/v1/models`, `/v1/messages`, and
  `/v1/messages/count_tokens`. Responses custom tools are converted to normal
  Anthropic tools with a typed raw-input wrapper and converted back before
  Codex executes them.
- Codex CLI as a provider uses `codex app-server` and is offered only to the
  Claude client; using Codex as both client and upstream would recurse.
- OpenCode CLI uses an isolated request-scoped provider configuration. Its
  project tools and context are not exposed as a second agent layer.

## Local development

Go 1.26.5 or newer is required.

Run from source:

```sh
go run ./cmd/macaz version
go run ./cmd/macaz claude
go run ./cmd/macaz codex
```

Build and run a local binary:

```sh
go build -o ./macaz ./cmd/macaz
./macaz version
./macaz codex
```

Run verification:

```sh
go mod verify
go test ./...
go vet ./...
go test -race ./...
```

Build a complete local release package into ignored `dist/`:

```sh
./scripts/build-release.sh v1.0.0
```

The release script requires an empty output directory and creates static
macOS, Linux, and Windows binaries for amd64 and arm64, plus checksums and the
installer. To choose another empty directory:

```sh
OUTPUT_DIR=/tmp/macaz-release ./scripts/build-release.sh v1.0.0
```

Opt-in live-provider tests never run in normal CI:

```sh
MACAZ_OPENAI_API_INTEGRATION_MODEL=<model> go test ./internal/provider/openai -run LiveOpenAIAPI
MACAZ_OPENAI_SUBSCRIPTION_INTEGRATION_MODEL=<model> go test ./internal/provider/openai -run LiveOpenAISubscription
MACAZ_OPENROUTER_INTEGRATION_MODEL=<provider/model> go test ./internal/provider/openrouter -run LiveOpenRouter
MACAZ_CODEX_INTEGRATION_EXECUTABLE=<codex-or-compatible> go test ./internal/provider/codexcli -run LiveCodex
MACAZ_OPENCODE_INTEGRATION_MODEL=<provider/model> go test ./internal/provider/opencodecli -run LiveOpenCode
MACAZ_CLAUDE_INTEGRATION=1 go test ./internal/app -run LiveClaudeLifecycle
```

Every merged pull request on `main` is verified, assigned the next SemVer tag,
built for all supported platforms, and published as a GitHub Release with
checksums, provenance, and generated release notes. The default bump is patch;
the `release:minor` and `release:major` labels select larger bumps.

## License

Apache License 2.0. See [LICENSE](LICENSE), [NOTICE](NOTICE), and
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
