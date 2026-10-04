# AI providers

The native agent engine imports this package directly. Requests use
`TranscriptContext`; streamed results retain thinking signatures, tool-call IDs,
usage and ordered content blocks without converting through `agentcore.ChatRequest`.

`MessageContent.Blocks` is a `*BlockList`: shallow copies retain the same list
when a provider or hook changes its entries or length. Use `Get`/`Len` to read
and `Set`/`Append`/`Delete`/`SetLength` to edit membership. `Values` returns a
detached slice of shared block pointers. `BlockReferences` creates a new list;
copy `MessageContent` to retain the existing list. Live access uses the owning
stream's synchronization; snapshots detach lists while preserving aliases.

Current integration scope:

| Provider | Native path |
| --- | --- |
| Codex | `StreamCodexResponsesPooled`, Responses SSE/WebSocket |
| Claude Code | `StreamClaudeCodePooled`, Anthropic SSE with CLI OAuth identity |
| Antigravity | `StreamAntigravityPooled`, Cloud Code SSE with per-account project |
| Gemini | `StreamOpenAICompletionsSimple`, Google OpenAI-compatible endpoint |

The host resolves endpoints and binds credentials to a provider row. OAuth pools
acquire/report per attempt and never store access tokens in transcripts. Auth
rotation stops after visible output. Host callback failures do not trigger
provider retries or account penalties. Antigravity retains the established
text/tool capabilities; parallel-call controls are explicitly unsupported.

Files are grouped by provider and concern within the Go package. Shared
transcript, event, HTTP and OAuth helpers have concrete data/callback contracts.
Existing legacy adapters and catalog helpers still serve public SDK, discovery
and compatibility callers, so they have not been deleted as unused.

Run `go test -race ./ai` for recorded Pi fixtures and local HTTP checks.
Provider text coverage includes 252 payload cases captured from unchanged
pinned Pi source, 21 local HTTP comparisons, and 12 Antigravity text cases
with a transport check. Claude HTTP expectations include the SDK's body
transformation. These checks preserve lone UTF-16 units in Codex instructions
and raw reasoning fields while filtering them from Pi's selected text fields.
TypeScript references and generators are removed; JSON fixtures, `LICENSE.pi`
and source hashes in `../third_party/pi/UPSTREAM.json` remain. The fixtures
establish tested behavior, not exhaustive equivalence to arbitrary JavaScript.
