# Privacy

macaz is a local interoperability tool. The project does not operate a hosted
model service, and the maintainers do not receive model prompts or responses by
default.

## Data flow

When macaz is running, the selected Claude Code or Codex CLI process connects to
a temporary authenticated listener on `127.0.0.1`. macaz translates the request
and sends it directly to the provider selected by the user. Depending on the
request, that provider may receive:

- user prompts and conversation history;
- system and developer instructions supplied by the selected client;
- tool names, descriptions, input schemas, calls, and results;
- source code, file contents, images, or documents included in context;
- model, token-usage, request, and technical metadata; and
- account credentials required by the selected provider.

The selected provider processes this data under its own terms and privacy
policy. OpenRouter may also route requests to an upstream model provider. Users
must review and configure the retention, training, regional-processing, and
data-control options offered by their provider.

## Local storage

macaz stores non-secret provider configuration in the platform configuration
directory. API keys and OpenAI authorization credentials are stored in the
operating-system credential store when available. Environment-variable
credentials remain process-level overrides.

Claude Code and Codex CLI use separate isolated profiles created for macaz.
Those profiles can contain client-owned conversation transcripts and other
client state. The Codex profile may link to user-managed skills, plugins,
prompts, and rules from the normal Codex profile, but it does not copy Codex
authentication into macaz. The local router does not independently persist
model request or response bodies.

`macaz reset claude` or `macaz reset codex` removes only that client's macaz
configuration and isolated profile. Shared provider credentials and the other
client remain. `macaz reset` without a client removes all macaz configuration,
both isolated profiles, their isolated session history, and all macaz-managed
provider credentials. These commands do not remove normal Claude/Codex profiles
or credentials managed directly by vendor clients and provider CLIs.

## Other network traffic

For the Claude Code child process, macaz requests do-not-track behavior and
disables error reporting, the feedback command, and the feedback survey. It
does not force `DISABLE_TELEMETRY` or
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`: those umbrella settings also disable
Claude Code feature-flag evaluation and can make client features unavailable.
This does not guarantee that a third-party client makes no other network
requests: feature evaluation, client update checks, safety services,
extensions, hooks, MCP servers, plugins, and other configured features may use
the network independently. In particular, Claude Code may send a WebFetch
hostname to Anthropic for a safety check even when model inference uses another
provider. Codex behavior outside the routed inference request follows the
user's Codex configuration and OpenAI's current documentation.

Provider CLIs, Claude Code, Codex CLI, OpenCode, the installer, MCP servers, and
GitHub may create their own local state or network logs under their respective
policies. macaz does not control those independent systems.

## Project telemetry

macaz contains no project-operated analytics or telemetry client. Download
hosts and source-code forges may still record ordinary access logs when users
download source code, release artifacts, or the installer.

Release builds make a short request to the official macaz GitHub Releases page
on each launch to determine the latest stable version. This request contains no
macaz configuration, credentials, prompts, responses, source code, or provider
data. As with any web request, GitHub can receive ordinary connection metadata
such as the user's IP address and the `macaz/<version>` user-agent. The check is
best-effort, produces no error when offline, does not download a binary, and can
be disabled with `MACAZ_NO_UPDATE_CHECK=1`.

`macaz update` queries the official GitHub release API and downloads
`SHA256SUMS` plus the platform-specific release binary from GitHub. These
requests are subject to GitHub's logging and privacy practices. macaz does not
send project or provider data as part of the update process.

## Security

The local routing layer binds only to loopback and uses a new random authentication
token for every launch. Configuration and isolated-profile paths use private
permissions where supported. Secrets should never be committed to source code,
included in issue reports, or shared in diagnostic output.

Users remain responsible for deciding whether their data may be sent to the
selected provider and for meeting any privacy, confidentiality, employment, or
regulatory obligations that apply to that data.

Anthropic documents Claude Code storage and network behavior in its
[Claude Code data usage documentation](https://code.claude.com/docs/en/data-usage)
and describes its own processing in the
[Anthropic Privacy Policy](https://www.anthropic.com/legal/privacy). The selected
provider's current privacy policy applies to data sent to that provider. OpenAI
publishes its current [Privacy Policy](https://openai.com/policies/privacy-policy/)
and [Codex documentation](https://developers.openai.com/codex/).
