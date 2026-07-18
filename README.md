# codex-opencode-executor

Use OpenCode models as durable external coding executors controlled by Codex.

`codex-opencode-executor` is a local STDIO MCP server that connects Codex to the
[OpenCode headless server](https://opencode.ai/docs/server/). Codex remains the
orchestrator and decision-maker while OpenCode owns the delegated model session,
context, and coding tools.

The default model is `xai/grok-4.5`, but any provider/model available in your
OpenCode installation can be selected globally or per session.

> [!IMPORTANT]
> This is an independent, pre-1.0 project. It is not affiliated with OpenAI,
> OpenCode, or xAI. The MCP orchestration, persistence, and Git workspace layers
> are project features; they are not native OpenCode features.

## Why this exists

Native Codex subagents are tightly integrated with Codex's own agent pool.
OpenCode executors are different: they have independent model context and model
usage, and communicate with Codex through MCP tool calls.

This project provides the missing orchestration layer:

- OpenCode session creation with configurable model, agent, and permissions
- asynchronous submission, polling, cancellation, and deadlines
- durable job state and restart reconciliation
- idempotent submissions
- automatic Git worktree isolation
- changed-file, commit, diff, and verification reporting
- explicit cleanup that protects active or modified workspaces
- local managed or remote OpenCode server modes

## Architecture

```text
Codex
  |
  | STDIO MCP
  v
codex-opencode-executor
  |-- durable job and workspace state
  |-- Git worktrees and verification
  |
  | OpenCode HTTP API
  v
OpenCode server
  |
  v
Configured provider/model (for example xAI Grok 4.5)
```

OpenCode performs delegated model work in its own session context. Codex only
receives the MCP request/response payloads it asks for. Compact status reports
and bounded diffs keep routine orchestration from flooding the Codex context.

This is an MCP integration, not ACP, and it does not add OpenCode to Codex's
native subagent pool.

## Requirements

- [Codex CLI](https://github.com/openai/codex) with local STDIO MCP support
- [OpenCode](https://opencode.ai/) installed and already configured with the
  provider/model you intend to use
- Git
- the Go version declared in `go.mod` when building from source

Provider authentication remains owned by OpenCode. This project does not
configure, replace, or migrate OpenCode provider credentials.

## Build

```bash
git clone https://github.com/pradhankukiran/codex-opencode-executor.git
cd codex-opencode-executor
go build -o codex-opencode-executor ./cmd/codex-opencode-executor
```

Run the test suite:

```bash
go test ./...
go vet ./...
go test -race ./...
```

## Connect it to Codex

### Recommended: managed local OpenCode server

The executor starts `opencode serve` as a child process on an available local
port. It inherits the current environment and normal OpenCode home directory,
so it uses your existing OpenCode configuration and provider authentication.

Use an absolute path to the built binary:

```bash
codex mcp add codex-opencode-executor \
  --env OPENCODE_MODE=local \
  --env CODEX_OPENCODE_EXECUTOR_DEFAULT_MODEL=xai/grok-4.5 \
  --env CODEX_OPENCODE_EXECUTOR_PERMISSION_MODE=inherit \
  --env CODEX_OPENCODE_EXECUTOR_ISOLATION_MODE=auto \
  -- /absolute/path/to/codex-opencode-executor
```

Confirm the registration:

```bash
codex mcp get codex-opencode-executor --json
codex mcp list
```

Restart Codex after changing MCP configuration. In the Codex TUI, `/mcp` shows
the connected server and its tools.

### Manual `config.toml`

Codex stores MCP configuration in `~/.codex/config.toml`. A project-scoped
`.codex/config.toml` can also be used for trusted projects.

```toml
[mcp_servers.codex-opencode-executor]
command = "/absolute/path/to/codex-opencode-executor"
args = [
  "-mode", "local",
  "-default-model", "xai/grok-4.5",
  "-permission-mode", "inherit",
  "-isolation-mode", "auto",
]
startup_timeout_sec = 20
tool_timeout_sec = 900
default_tools_approval_mode = "writes"
```

See the official [Codex MCP documentation](https://developers.openai.com/codex/mcp/)
for all supported MCP configuration fields.

### Connect to an existing OpenCode server

OpenCode documents `opencode serve` as its standalone headless HTTP mode. Start
it separately when you want the server lifecycle to remain outside this MCP
process:

```bash
OPENCODE_SERVER_PASSWORD='choose-a-password' \
  opencode serve --hostname 127.0.0.1 --port 4096
```

Then register the executor in remote mode:

```bash
codex mcp add codex-opencode-executor \
  --env OPENCODE_MODE=remote \
  --env OPENCODE_URL=http://127.0.0.1:4096 \
  --env OPENCODE_PASSWORD=choose-a-password \
  --env CODEX_OPENCODE_EXECUTOR_DEFAULT_MODEL=xai/grok-4.5 \
  -- /absolute/path/to/codex-opencode-executor
```

`OPENCODE_SERVER_PASSWORD` protects the OpenCode server. `OPENCODE_PASSWORD`
is the corresponding client credential used by this executor. The default
username is `opencode`; override it with `OPENCODE_SERVER_USERNAME` on the
server and `OPENCODE_USERNAME` on the executor.

Do not expose an unauthenticated OpenCode server on an untrusted network.

## Typical orchestration flow

1. Codex calls `handoff_create_session` with the project directory and optional
   model, agent, permission, or isolation overrides.
2. Codex calls `handoff_fire` with a bounded implementation task.
3. Codex polls `handoff_check`; OpenCode continues working in its own context.
4. Codex inspects `handoff_workspace` and requests `handoff_diff` when needed.
5. Codex calls `handoff_verify` with explicit executable/argument checks.
6. Codex reviews and integrates the worktree branch or patch.
7. Codex calls `handoff_cleanup` only after the result is safely integrated.

The executor intentionally does not merge delegated changes into the source
branch automatically. Codex remains responsible for review and integration.

## MCP tools

| Tool | Purpose |
| --- | --- |
| `handoff_health` | Check OpenCode server connectivity |
| `handoff_agents` | List available OpenCode agents |
| `handoff_models` | List and filter providers/models |
| `handoff_sessions` | List OpenCode sessions |
| `handoff_create_session` | Create a model session and bind its workspace |
| `handoff_fire` | Submit a durable asynchronous job |
| `handoff_check` | Poll execution and receive a compact workspace report |
| `handoff_cancel` | Abort active execution |
| `handoff_workspace` | Inspect changed files, commits, diff stats, and checks |
| `handoff_diff` | Read a bounded tracked-file Git diff |
| `handoff_verify` | Run and record explicit argv-based verification commands |
| `handoff_cleanup` | Remove an executor-owned worktree safely |
| `handoff_permission_reply` | Answer an OpenCode permission request |
| `handoff_question_reply` | Answer or reject an OpenCode clarification question |

## Models and agents

The server default is configurable:

```bash
codex-opencode-executor \
  -default-model xai/grok-4.5 \
  -default-agent build
```

Equivalent environment variables:

```text
CODEX_OPENCODE_EXECUTOR_DEFAULT_MODEL=xai/grok-4.5
CODEX_OPENCODE_EXECUTOR_DEFAULT_AGENT=build
```

The model uses OpenCode's `provider/model` form. A caller can override it with
the `model` field when creating a session. The selected model is fixed for that
session.

Use `handoff_models` to discover the providers and models visible to the active
OpenCode installation rather than assuming a model is available.

## Permission modes

The executor supports four permission modes:

| Mode | Behavior |
| --- | --- |
| `inherit` | Preserve OpenCode's configured permission behavior |
| `ask` | Ask before OpenCode tool actions |
| `deny` | Deny OpenCode tool actions |
| `yolo` | Set the OpenCode catch-all permission to `allow` |

Configure the default with `-permission-mode` or
`CODEX_OPENCODE_EXECUTOR_PERMISSION_MODE`. `-yolo` is a shortcut for
`-permission-mode=yolo`.

`yolo` is intentionally explicit and is not the same as OpenCode's `--auto`
behavior. OpenCode's documented auto mode still honors explicit deny rules;
this project's `yolo` mode sets the catch-all rule to allow.

The permission mode controls OpenCode actions. `handoff_verify` is different:
it is an explicit MCP operation executed by this server in the bound workspace,
using a command plus an argument array without implicit shell expansion. Codex
MCP approval policy should protect that tool appropriately.

Read the [OpenCode permission documentation](https://opencode.ai/docs/permissions/)
before enabling unattended execution.

## Workspace isolation

Configure isolation globally with `-isolation-mode` or
`CODEX_OPENCODE_EXECUTOR_ISOLATION_MODE`, and override it per session.

| Mode | Behavior |
| --- | --- |
| `auto` | Use a new worktree for clean Git projects; run directly for dirty or non-Git directories |
| `worktree` | Require a clean Git project and create a new worktree; otherwise return an error |
| `none` | Run directly in the requested directory |

`auto` avoids silently omitting uncommitted source changes. Executor-owned
worktrees receive branches named `codex-opencode-executor/<id>` and remain on
disk until explicit cleanup.

Cleanup refuses to run while a tracked job is active. It also refuses to remove
a worktree containing changes or commits unless `force=true` is supplied.
Forced cleanup discards that worktree and branch, so integrate valuable work
first.

## Durability

By default, state is stored below the operating system's user cache directory:

```text
codex-opencode-executor/jobs
codex-opencode-executor/workspaces
codex-opencode-executor/worktrees
```

Override the locations with:

```text
CODEX_OPENCODE_EXECUTOR_STATE_DIR
CODEX_OPENCODE_EXECUTOR_WORKSPACE_STATE_DIR
CODEX_OPENCODE_EXECUTOR_WORKTREE_DIR
```

Job records are written atomically. Jobs that were active during an executor
restart are recovered as unknown and reconciled against the OpenCode session.
Deadlines trigger an OpenCode abort attempt and a terminal `timed_out` state.

`handoff_fire` accepts an idempotency key scoped to its session. Retrying the
same request with the same key returns the existing job; reusing the key with
different input is rejected.

## Configuration reference

Run `codex-opencode-executor -help` for the authoritative flag list.

| Flag | Environment variable | Default |
| --- | --- | --- |
| `-mode` | `OPENCODE_MODE` | local when no URL is configured |
| `-opencode-url` | `OPENCODE_URL` | managed local server |
| `-opencode-username` | `OPENCODE_USERNAME` | `opencode` |
| `-opencode-password` | `OPENCODE_PASSWORD` | empty |
| `-default-directory` | `OPENCODE_DIRECTORY` | empty |
| `-default-model` | `CODEX_OPENCODE_EXECUTOR_DEFAULT_MODEL` | `xai/grok-4.5` |
| `-default-agent` | `CODEX_OPENCODE_EXECUTOR_DEFAULT_AGENT` | OpenCode default |
| `-permission-mode` | `CODEX_OPENCODE_EXECUTOR_PERMISSION_MODE` | `inherit` |
| `-isolation-mode` | `CODEX_OPENCODE_EXECUTOR_ISOLATION_MODE` | `auto` |
| `-state-dir` | `CODEX_OPENCODE_EXECUTOR_STATE_DIR` | user cache |
| `-workspace-state-dir` | `CODEX_OPENCODE_EXECUTOR_WORKSPACE_STATE_DIR` | user cache |
| `-worktree-dir` | `CODEX_OPENCODE_EXECUTOR_WORKTREE_DIR` | user cache |
| `-request-timeout` | — | `30s` |
| `-sync-timeout` | — | `5m` |

`-opencode-env KEY=VALUE` and `-opencode-arg VALUE` may be repeated to customize
the managed local OpenCode process.

## Current limitations

- Real compatibility depends on the installed OpenCode server version and its
  evolving API.
- The vendored generated bindings come from the upstream project's OpenAPI
  snapshot. The documented abort route uses a small direct HTTP adapter because
  that snapshot predates the endpoint.
- Full diffs cover tracked files. Workspace reports include untracked file names,
  but not their contents.
- Managed local server startup does not yet perform a readiness handshake before
  the first request.
- Automated end-to-end coverage against real provider subscriptions is still in
  progress; local tests use isolated Git repositories and OpenCode server fakes.

## Security notes

- Keep OpenCode bound to localhost unless you have deliberately secured the
  network path.
- Use server authentication in remote mode.
- Treat `yolo`, `handoff_verify`, and forced cleanup as privileged operations.
- Review delegated changes and verification output before integration.
- API body logging may expose prompts or model output; enable `-log-api` only for
  controlled debugging.

## Provenance

This project was derived from the
[`opencode-handoff-mcp`](https://github.com/go-faster/gooners/tree/main/cmd/opencode-handoff-mcp)
component in `go-faster/gooners`. See `NOTICE` for attribution.

## License

MIT. See `LICENSE`.
