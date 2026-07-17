# Privacy

macaz is a local interoperability tool. The project does not operate a hosted
model gateway, and the maintainers do not receive model prompts or responses by
default.

## Data flow

When macaz is running, Claude Code connects to a temporary authenticated
listener on `127.0.0.1`. macaz translates the request and sends it directly to
the provider selected by the user. Depending on the request, that provider may
receive:

- user prompts and conversation history;
- system instructions supplied by Claude Code;
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

Claude Code uses an isolated profile created for macaz. That profile can
contain local conversation transcripts and other Claude Code state. macaz's
gateway does not independently persist model request or response bodies.

`macaz reset` removes macaz configuration, macaz-managed provider credentials,
the isolated Claude profile, and the session history stored in that profile. It
does not remove the normal Claude Code profile or credentials managed directly
by provider CLIs.

## Other network traffic

macaz disables Claude Code telemetry, error reporting, feedback commands, and
feedback surveys in the child process it starts. This does not guarantee that
Claude Code makes no other network requests: update checks and safety-related
requests can remain. In particular, Claude Code may send a WebFetch hostname to
Anthropic for a safety check even when model inference uses another provider.

Provider CLIs, Claude Code, the installer, and GitHub may create their own local
state or network logs under their respective policies. macaz does not control
those independent systems.

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

The local gateway binds only to loopback and uses a new random authentication
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
provider's current privacy policy applies to data sent to that provider.
