# macaz

[![CI](https://github.com/macaz-dev/macaz-cli/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/macaz-dev/macaz-cli/actions/workflows/ci.yml)

macaz keeps Claude Code as the local coding client while routing model inference
through OpenAI, OpenRouter, Codex CLI, or OpenCode CLI.

macaz is free and open-source software licensed under Apache-2.0. Every build
has the complete feature set.

> [!IMPORTANT]
> macaz is an independent interoperability project. It is not affiliated with,
> authorized by, endorsed by, or sponsored by Anthropic or any model provider.
> Claude Code must be installed separately from an authorized source, and use
> of Claude Code and each provider remains subject to their respective terms.

See the [legal and compatibility notice](LEGAL.md) and [privacy disclosure](PRIVACY.md)
before use.

```text
macaz
```

Claude still executes files, shell commands, edits, skills, MCP tools, hooks,
images, attachments, and agents on your machine. macaz uses only a temporary
authenticated loopback connection and preserves Claude's normal permission
prompts by default.

## Install

On macOS or Linux, install the latest GitHub release with:

```sh
curl -fsSL https://raw.githubusercontent.com/macaz-dev/macaz-cli/main/scripts/install.sh | sh
```

The installer detects the platform, downloads the matching release binary,
requires a matching entry in `SHA256SUMS`, verifies SHA-256, and installs to
`$HOME/.local/bin` by default. Pin a release with `--version`:

```sh
curl -fsSL https://raw.githubusercontent.com/macaz-dev/macaz-cli/main/scripts/install.sh |
  sh -s -- --version 0.1.0
```

Windows binaries and all other release files are available from
[GitHub Releases](https://github.com/macaz-dev/macaz-cli/releases).

To opt into Claude Code's full-permission mode:

```text
macaz --dangerously-skip-permissions
```

## Setup

On first launch:

```text
1. OpenAI Subscription
2. OpenAI API
3. OpenRouter API
4. Codex-CLI
5. OpenCode-CLI
```

The selection is saved. API keys and OAuth credentials are stored in the
operating-system credential store, not in `config.json`.

Requirements:

- Claude Code 2.1.211 or newer, with `claude` on `PATH`;
- Codex CLI 0.144.5 or newer for Codex-CLI, or a compatible custom executable
  such as `wcodex`;
- OpenCode CLI 1.18.3 or newer for OpenCode-CLI, or a compatible custom
  executable;
- a browser for the normal OpenAI Subscription device authorization flow.

OpenAI and OpenRouter setup ask for the API key. OpenAI Subscription displays a
URL and code, opens the browser, waits for confirmation, validates the account,
and only then starts Claude.

## Commands

```text
macaz [claude arguments...]
macaz status
macaz doctor
macaz reset
macaz legal
macaz update
macaz version
```

Release builds perform a short, best-effort check against the official GitHub
Releases page on each start. If a newer stable release exists, macaz prints a
notice to standard error but never installs it automatically. Network failures
remain silent and do not prevent the requested command from running. Disable
this check with:

```sh
export MACAZ_NO_UPDATE_CHECK=1
```

Install the latest release in place with:

```text
macaz update
```

The updater downloads only the exact GitHub asset for the current operating
system and architecture, requires its entry in `SHA256SUMS`, checks GitHub's
asset digest when available, executes the staged binary to confirm its embedded
version, and then replaces the current executable with rollback protection. It
does not send configuration, credentials, prompts, or provider data. The user
running macaz must have write permission to the installed binary's directory;
otherwise, use the installer with suitable permissions.

Every provider-backed `macaz` start refreshes the provider catalog
automatically, so newly available models appear in Claude's `/model` interface.
Provider, model, and effort selection stay in the initial setup and Claude
interface rather than adding duplicate macaz commands.

For the official OpenAI API, `/v1/models` remains the authority for
availability. macaz uses public models.dev metadata only to hide non-agentic
entries and enrich capability labels. Startup falls back safely if models.dev
is unavailable.

`macaz reset` removes macaz provider configuration, provider credentials, the
isolated Claude profile, and macaz sessions. It does not delete normal Claude
Code or vendor CLI credentials.

## Isolation

macaz uses a dedicated Claude profile and passes connection variables only to
the child process it starts. Therefore:

- running `claude` normally does not use macaz;
- macaz sessions do not remain in normal Claude history;
- the macaz daemon is stopped when Claude exits;
- Claude cannot silently retry or fall back to an official Anthropic model;
- provider-native CLI context and tools are disabled or replaced.

The large welcome header collapsing to a smaller header after the first prompt
is normal Claude Code UI behavior. The model shown in either header must remain
the selected macaz provider model.

## Provider Notes

- OpenAI API and OpenRouter use Responses-compatible HTTP APIs.
- OpenAI Subscription uses device OAuth, refreshes credentials automatically,
  serializes requests, and applies bounded retry/backoff for account rate
  limits. Images remain native inputs; PDF and text documents are extracted
  locally with strict size bounds because the account Codex endpoint does not
  publish the public API's generic `input_file` contract. It is experimental
  and remains subject to the provider's terms.
- Codex-CLI uses `codex app-server`, client-supplied instructions, and dynamic
  tools. It is the strongest local CLI bridge.
- OpenCode-CLI uses an isolated request-scoped plugin/configuration because
  OpenCode has no equivalent stable dynamic-tool protocol. It remains
  experimental. Its native provider-auth plugins remain available so existing
  OpenCode logins work, while project context, provider prompts, skills, MCP
  configuration, and built-in tools remain excluded from model requests.
- OpenCode model labels include their upstream provider. For example,
  `OpenAI / GPT-5.4` uses the OpenAI account configured in OpenCode, while
  `OpenCode Zen / GPT-5.4` uses OpenCode Zen and may require separate billing.

No adapter can make different proprietary models semantically identical.
macaz's compatibility target is preserving Claude Code's local client
functionality and translating provider interactions without hidden fallback.

## Local Development

Go 1.26.5 or newer is required.

Run directly from source (the reported version is `dev`):

```sh
go run ./cmd/macaz version
go run ./cmd/macaz
```

Development builds do not perform update checks and cannot self-update. The
feature is enabled only when the build contains a release version such as
`v1.0.0`.

Build and run a local binary. The root-level output is ignored by Git:

```sh
go build -o ./macaz ./cmd/macaz
./macaz version
./macaz
```

Run the local verification suite:

```sh
go mod verify
go test ./...
go vet ./...
go test -race ./...
```

Build the complete local release package with an explicit version:

```sh
./scripts/build-release.sh v1.0.0
```

The release script creates all six static binaries under ignored `dist/`:
macOS, Linux, and Windows for both amd64 and arm64. Every binary has the same
unrestricted functionality. The output directory must be empty, so stale
artifacts cannot enter a release. Use `OUTPUT_DIR` for another empty location:

```sh
OUTPUT_DIR=/tmp/macaz-release ./scripts/build-release.sh v1.0.0
```

GitHub Releases use this same script.

The release package also contains the canonical `scripts/install.sh`. It
installs either the latest GitHub release or a version selected with
`--version`:

```text
curl -fsSL https://raw.githubusercontent.com/macaz-dev/macaz-cli/main/scripts/install.sh | sh
```

Real providers use explicit opt-in smoke tests, so normal CI never consumes
credentials or provider quota. The API-provider tests validate text, forced
client tool calls, images when the selected model advertises vision, and
documents when the provider path supports them:

```text
MACAZ_OPENAI_API_INTEGRATION_MODEL=<model> go test ./internal/provider/openai -run LiveOpenAIAPI
MACAZ_OPENAI_SUBSCRIPTION_INTEGRATION_MODEL=<model> go test ./internal/provider/openai -run LiveOpenAISubscription
MACAZ_OPENROUTER_INTEGRATION_MODEL=<provider/model> go test ./internal/provider/openrouter -run LiveOpenRouter
MACAZ_CODEX_INTEGRATION_EXECUTABLE=<codex-or-wcodex> go test ./internal/provider/codexcli -run LiveCodex
MACAZ_OPENCODE_INTEGRATION_MODEL=<provider/model> go test ./internal/provider/opencodecli -run LiveOpenCode
MACAZ_CLAUDE_INTEGRATION=1 go test ./internal/app -run LiveClaudeLifecycle
```

Release builds are static (`CGO_ENABLED=0`) and target:

- Linux `amd64`, `arm64`
- macOS `amd64`, `arm64`
- Windows `amd64`, `arm64`

Every PR merged into `main` runs release verification, reserves the next
SemVer tag, builds all six binaries, generates checksums and provenance, and
publishes a GitHub Release with generated release notes. The default bump is
`patch`; add a `release:minor` or `release:major` PR label for that bump. The
first default release is `v0.1.0`. A rerun reuses the tag already attached to
the merge commit instead of creating another release version.

## Security

- Random `127.0.0.1` port and random token per launch.
- Secrets in the OS credential store.
- Private config/profile files.
- Bounded request, response, and attachment sizes.
- Context cancellation and timeouts.
- No macaz prompt or response persistence.
- Claude Code telemetry, error reporting, feedback, and surveys disabled in the
  macaz child process.

## License

macaz is licensed under the [Apache License 2.0](LICENSE).
Third-party attributions are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
