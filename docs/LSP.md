# Language intelligence (LSP)

AgentRay's selectable `lsp` tool provides read-only, symbol-aware code
intelligence over the standard Language Server Protocol:

- file diagnostics;
- document symbols;
- hover information;
- go to definition;
- find references.

It uses the same workspace and isolation boundary as `run_shell`. On a local
laptop, `HostSandbox` starts the configured binary without inheriting AgentRay's
environment. On a server with sandboxing enabled, the binary runs inside a
hardened container process with no network, a read-only workspace mount, resource
limits, and an isolated temporary home. If the deployment requires a sandbox
but no interactive-process backend is available, the catalog withholds `lsp`.

"Read-only" describes the operations exposed to the model. In local host mode,
the language server is an operator-trusted process and the OS does not enforce a
read-only workspace or network boundary. Use only trusted local binaries. In
multi-tenant or untrusted deployments, require `DockerSandbox`; it enforces the
read-only mount, network denial, and resource limits.

## Configuration

Language servers are operator-provisioned. The tool never downloads or installs
a binary. Enable `lsp` for an agent and store configuration like:

```json
{
  "timeout_seconds": 20,
  "servers": [
    {
      "name": "gopls",
      "command": "gopls",
      "args": ["serve"],
      "extensions": [".go"],
      "language_id": "go",
      "image": "your-registry/agentray-gopls:stable",
      "settings": {
        "gopls": {
          "staticcheck": true
        }
      }
    }
  ]
}
```

`image` is optional. Local host mode ignores it; Docker mode uses it instead of
the default sandbox image. The selected image must contain the configured
binary and its runtime. A laptop may omit `image` and use a binary already on
`PATH`.

Server fields:

| Field | Required | Meaning |
|---|---:|---|
| `name` | yes | Unique diagnostic name |
| `command` | yes | Executable on the laptop or inside `image` |
| `args` | no | Server arguments, normally including its stdio mode |
| `extensions` | yes | Lower/upper-case file extensions handled by this server |
| `language_id` | no | LSP language id; inferred for common extensions |
| `diagnostics_only` | no | Include for diagnostics, exclude from navigation |
| `image` | no | Server-side sandbox image override |
| `initialization_options` | no | Value sent during `initialize` |
| `settings` | no | Value sent with `workspace/didChangeConfiguration` |

Up to 20 servers may be configured. Diagnostics query every matching server and
bound their results; navigation uses the first matching server
that is not `diagnostics_only`.

## Model-facing contract

The tool accepts `status`, `diagnostics`, `document_symbols`, `hover`,
`definition`, and `references`. Position-based actions use a 1-based line plus
the exact symbol substring rather than asking the model to calculate UTF-16
columns. When a substring occurs multiple times, append `#N`, such as
`target#2`.

Initialized servers are retained in a process-local, bounded registry. A client
key includes tenant/project/agent namespace, conversation, workspace, host or
sandbox mode, and the complete server configuration. Calls to one client are
serialized; unrelated clients can run concurrently. The default soft capacity
is four clients, idle clients expire after five minutes, and every process has a
one-hour hard lifetime. Active clients are never evicted to meet capacity.

Before every action the tool rereads the target through the guarded workspace
filesystem, clears that URI's cached diagnostics, and opens the current text at
a new document version. It closes the document after the query. Cancellation
or any uncertain protocol failure
discards the client without replaying the action. A replica miss simply starts
a clean client, so server deployments do not require shared state or sticky
routing. Local and Docker deployments use the same registry contract; only the
process backend differs.

The connection continues servicing server-initiated requests while idle. It
returns configured settings and workspace folders, tracks dynamic capability
registration, rejects read-only workspace edits, acknowledges headless
progress/refresh requests, and returns JSON-RPC method-not-found for unsupported
requests. Responses are routed directly to the requesting call instead of
through a shared bounded queue, so late or unknown replies cannot stall the
reader. One writer pump bounds a wedged stdin pipe to one goroutine; cancelled
writes do not accumulate behind it.

Diagnostics carrying an older document version cannot overwrite a newer
publication. Exact-version publications are accepted immediately; unversioned
publications must settle briefly so an in-flight pre-edit result can be
superseded. Pull diagnostics are considered authoritative only for a valid
`full` report—an `unchanged` response without cached result state is unknown,
never a clean file. Protocol writes and graceful shutdown are time-bounded so a
server that stops reading cannot pin a cancelled tool call. Process resources
are released only after `Wait` confirms exit. If force-kill does not produce a
confirmed exit within the shutdown budget, the registry retains a same-key
closing tombstone: later calls fail closed instead of starting a second server
beside a possibly live one, and cleanup completes asynchronously when exit is
finally observed.

Host-mode file URIs are portable across Unix paths, Windows drive paths, and
UNC shares. Server-returned file URIs validate authority and credentials before
conversion, preserve literal percent-encoded filename bytes, and are still
resolved through the workspace escape guard. The isolated server path remains
the fixed `/workspace` form.

The registry is private to AgentRay rather than a cross-process mux. That keeps
authentication and tenant isolation inside the existing Runner boundary while
capturing the main benefit: language-server initialization and indexing are not
repeated for every tool call.

Rename, code actions, formatting, and file rename are intentionally absent from
this read-only version. They require a snapshot-checked cross-file workspace
transaction so a server edit cannot partially overwrite concurrent user work.
