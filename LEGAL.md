# Legal and Compatibility Notice

macaz is an independent, community-maintained interoperability project. It is
not affiliated with, authorized by, endorsed by, or sponsored by Anthropic,
OpenAI, OpenRouter, or any other client, model, or service provider.

Claude and Claude Code are trademarks of Anthropic PBC. OpenAI and Codex are
trademarks of OpenAI, L.L.C. Other names and marks belong to their respective
owners. References to third-party products are solely to identify
compatibility and required external services. No ownership of those marks is
claimed.

## Independent software

The Apache-2.0 license applies only to the macaz source code and distributions
created from it. It does not license Claude Code, provider models, provider
APIs, third-party trademarks, or any other third-party software or service.

macaz does not include or redistribute Claude Code, Codex CLI, OpenCode, or any
model. Users must obtain every client separately from an authorized source.
macaz starts the selected external client without patching its executable and
uses configurable provider interfaces to translate model requests for a
provider selected by the user.

When Anthropic API is selected, macaz uses an API credential supplied by the
user and Anthropic's API endpoints. It does not convert, resell, or grant access
to a Claude consumer subscription. When OpenAI Subscription is selected, access
is performed by the user's separately authorized account and remains subject to
OpenAI's applicable terms and product availability.

Compatibility with a product or protocol does not imply approval by its owner.
No representation is made that a third party permits every possible use of its
software, account, subscription, API, model, or service.

## User responsibilities

Users are responsible for:

- having authority to install and use each selected client and provider;
- complying with the current terms, acceptable-use policies, organizational
  policies, licenses, and fees applicable to those products and services;
- ensuring that macaz use is permitted by their employer or organization;
- having the rights and permissions needed for prompts, source code, files,
  personal data, and other content sent to a provider;
- reviewing generated output and tool calls before relying on them; and
- complying with export controls, sanctions, privacy, intellectual-property,
  security, and other applicable laws.

macaz does not grant access to any third-party account, subscription, model, or
service. It must not be used to evade authentication, usage limits, payment,
access controls, safety controls, or organizational policy.

## Provider behavior and availability

Providers receive and process request content according to their own terms and
privacy practices. They may charge fees, impose limits, retain data, change
interfaces, reject requests, or discontinue access. Different models are not
semantically identical, and compatibility does not guarantee identical output,
tool behavior, safety behavior, availability, or performance.

Third-party interfaces and terms can change without notice. The maintainers do
not guarantee continued compatibility with any client, model, or provider.

## Warranty and liability

macaz is provided under the Apache License 2.0 on an "AS IS" basis, without
warranties or conditions of any kind. The warranty disclaimer and limitation of
liability in the [LICENSE](LICENSE) apply to the maximum extent permitted by
law.

This notice describes the project's intended boundaries; it is not legal advice
and does not replace the terms between a user and a third-party provider.

For convenience, Anthropic's current public documents include its
[Commercial Terms](https://www.anthropic.com/legal/commercial-terms),
[Consumer Terms](https://www.anthropic.com/legal/consumer-terms), and
[Claude Code model-provider configuration documentation](https://code.claude.com/docs/en/llm-gateway).
OpenAI publishes its current
[Terms of Use](https://openai.com/policies/terms-of-use/) and
[Services Agreement](https://openai.com/policies/services-agreement/).
These links are provided for convenience, may change, and do not identify which
terms apply to a particular user; the agreement applicable to that user and use
case controls.
