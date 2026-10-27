# ACP agents: run tasks and workspace setup on Claude Code / Codex

Date: 2026-10-27. Fork: `yangbooom/ax`, branch `feat/acp-agents`.

## Intent

Make AX use the LLM access we already pay for (a Claude subscription and a
ChatGPT/Codex subscription) instead of a Gemini API key. The way in is the
[Agent Client Protocol](https://agentclientprotocol.com): both vendors ship an
ACP adapter for their coding agent, so one Go ACP client in the runner can
drive either agent over stdio.

Two things inside a sandbox call a model today, and both move to ACP:

1. **Workspace goal setup.** `Task.spec.workspace.goal` is handed to an agent
   on the maiden run to finish preparing the workspace (install toolchains,
   dependencies, and so on).
2. **The task body.** Today a Task runs `spec.command`. A Task will also be
   able to run an *agent with a prompt* instead of a command.

## Goals

- A `Task` can declare `spec.agent.prompt`, and the runner runs Claude Code or
  Codex against that prompt in the workspace, as the task body.
- Workspace `goal` setup runs on the same agent kind when one is configured.
- Which agent, which model id, and which credential come from a `Model`
  resource, so switching vendors or rotating a token is one `ax apply` for
  every task created afterwards (existing actors keep the environment they
  were created with, exactly as with `GEMINI_API_KEY` today).
- Credentials are subscription credentials stored in a Kubernetes Secret,
  resolved by the control plane exactly the way `GEMINI_API_KEY` is today.
- The upstream Gemini/Antigravity path keeps working unchanged when no ACP
  model is configured, so the fork stays mergeable with `google/ax`.

## Non-goals

- Multi-turn or interactive sessions. One prompt, one turn, done. Steering a
  running agent is `ax ssh`.
- Feeding the agent's result back to the control plane. The control plane
  does not read command exit codes today; it will not read agent stop reasons
  either. The runner logs them and reports through the existing
  `OnCommandExit` hook.
- Other ACP agents (Gemini CLI, Goose, ...). The design makes adding one a
  table entry, but only Claude Code and Codex ship.
- API-key authentication for Claude or Codex. It happens to work (the
  adapters read `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` from the environment,
  which `spec.env` can set), but nothing is built for it.
- Keeping the Codex refresh token in sync across sandboxes (see Risks).
- Removing Antigravity from the default image.

## Anti-goals

- Any change that makes a Task without `spec.agent` and an atespace without an
  ACP `Model` behave differently from upstream.
- Credentials in the Task or Workspace YAML, in `ax get` output, or in logs.
- The runner exiting when the agent finishes. The sandbox must stay
  inspectable, as it does after a command exits.

## Constraints

- Go 1.27, protobuf-generated API types in `pkg/apis/v1alpha1` (regenerate
  with `protoc` + `protoc-gen-go`; no Makefile target exists, add one).
- The runner image is `Dockerfile.task-runner` (python:3.12-slim). The two
  adapters are npm packages that need Node 22+.
- The control plane passes everything to the container as environment
  variables on a per-task Substrate ActorTemplate; the template name is a
  digest of image + env, so a credential change provisions a new template.
  That is how `GEMINI_API_KEY` ships today and this design does the same.
- `/ax` inside the container is not durable across a volumes-only resume
  (upstream issue google/ax#518). Anything the runner writes there must be
  re-creatable from the environment on every boot.

## Design

### API (`pkg/apis/v1alpha1/ax.proto`)

```proto
message TaskSpec {
  // ...existing fields 1-6...
  // agent runs an ACP agent with a prompt as the task body instead of
  // spec.command. Exactly one of command and agent may be set.
  AgentSpec agent = 7;
}

message AgentSpec {
  // model names the Model resource in the task's atespace whose provider
  // runs this task. Empty means "default-model".
  string model = 1;
  // prompt is the single user turn sent to the agent, run with the
  // workspace path as its working directory.
  string prompt = 2;
}
```

`ModelSpec` gains no fields. `provider` gets two new accepted values:

| `provider`    | Agent        | Adapter (npm)                             | `secretKey.key` holds                                   |
|---------------|--------------|-------------------------------------------|---------------------------------------------------------|
| `claude-code` | Claude Code  | `@agentclientprotocol/claude-agent-acp`   | the token printed by `claude setup-token`               |
| `codex`       | Codex        | `@agentclientprotocol/codex-acp`          | the contents of `$CODEX_HOME/auth.json` after `codex login` |

`model` is the vendor model id (`claude-opus-5-5`, `gpt-5.5-codex`, ...) and is
optional; empty lets the agent pick its default.

Validation at `ax apply` (server side, where `ValidateResources` runs):

- `spec.command` and `spec.agent` are mutually exclusive.
- `spec.agent` requires a non-empty `prompt`.

Example manifests (`examples/agent-task.yaml`):

```yaml
apiVersion: ax.io/v1alpha1
kind: Model
metadata: {name: default-model}
spec:
  provider: claude-code
  model: claude-opus-5-5
  secretKey: {name: claude-subscription, key: CLAUDE_CODE_OAUTH_TOKEN}
---
apiVersion: ax.io/v1alpha1
kind: Task
metadata: {name: fix-flaky}
spec:
  workspace: {name: golang, goal: "Make sure the Go toolchain builds from source"}
  agent:
    prompt: "Find and fix the flaky test in ./internal/lock. Commit on a branch."
  debug: true
```

### Control plane (`internal/server`, `internal/controller/reconciler.go`)

The reconciler has no store access. It gets a `ModelResolver func(ctx,
atespace, name) (*v1alpha1.Model, error)` field next to the existing
`SecretResolver`; `cmd/ax-server` sets it to a closure over the store, tests
replace it. The `Reconciler` interface and its call sites do not change.

Today `Reconcile` builds `extraEnv` from `spec.env`, `GEMINI_API_KEY`,
`AX_TASK_YAML`, `AX_WORKSPACE_YAML`. It gains one step, **agent env**:

1. The Model is `task.spec.agent.model` if set, otherwise `default-model`
   in the task's atespace. Not found and `spec.agent` is set → task `Failed`
   with condition reason `ModelNotFound`. Not found and no `spec.agent` →
   nothing to do (upstream behavior). `spec.agent` set and the Model's
   provider is not an ACP one (`google`, empty, unknown) → `Failed`,
   `UnsupportedProvider`.
2. If the Model's `provider` is `claude-code` or `codex`, resolve its
   `secretKey` through the existing `SecretResolver` and add to `extraEnv`:

   | Key                | Value                                                      |
   |--------------------|------------------------------------------------------------|
   | `AX_AGENT`         | `claude-code` \| `codex`                                   |
   | `AX_AGENT_MODEL`   | `Model.spec.model` (may be empty; then omitted)            |
   | `CLAUDE_CODE_OAUTH_TOKEN` | the secret value, when provider is `claude-code`    |
   | `CODEX_AUTH_JSON`  | the secret value, when provider is `codex`                 |

   Secret missing and `spec.agent` set → task `Failed`, reason
   `CredentialNotFound`: an agent task without a credential can never do
   anything useful, so it fails loudly. Secret missing and no `spec.agent`
   (a plain command task whose atespace happens to have an ACP
   `default-model`) → log a warning, inject nothing, continue, so upstream
   style tasks behave as they do today.
3. Any other provider (`google`, empty) → unchanged: `lookupGeminiKey` as
   today.

The Gemini lookup still runs regardless, so a mixed atespace keeps working.

### Runner

New package **`internal/agent`** owns the ACP client. Public surface:

```go
type Kind string // "claude-code" | "codex"

type Spec struct {
    Kind   Kind
    Model  string // optional vendor model id
    Cwd    string // workspace path; the agent's session cwd
    Prompt string
    Env    []string // inherited by the adapter process
    Output io.Writer // human-readable stream of what the agent is doing
}

// Run starts the adapter, opens one ACP session in Cwd, sends Prompt as a
// single turn, streams updates to Output, and returns the stop reason once
// the turn ends. Cancelling ctx sends session/cancel, then SIGTERM, then
// SIGKILL to the adapter's process group.
func Run(ctx context.Context, s Spec) (acp.StopReason, error)

// FromEnv reads AX_AGENT / AX_AGENT_MODEL and reports whether an ACP agent
// is configured for this container.
func FromEnv() (kind Kind, model string, ok bool)
```

Internals, in order:

- **Adapter command.** `claude-code` → `claude-agent-acp`; `codex` →
  `codex-acp`. Both are on `PATH` in the default image. Started with
  `Setpgid` like the task command, stderr passed through to the runner's
  stderr, stdin/stdout are the ACP pipes.
- **Per-kind environment.**
  - `claude-code`: `ANTHROPIC_MODEL=<model>` when set. The token is already in
    the environment as `CLAUDE_CODE_OAUTH_TOKEN`; the Claude Code CLI inside
    the adapter reads it natively. `HOME` must be writable (the CLI writes
    `~/.claude`); the runner sets `HOME=/ax/home` if `HOME` is unset or not
    writable.
  - `codex`: `CODEX_HOME=/ax/codex`, created on every boot; `CODEX_AUTH_JSON`
    is written to `$CODEX_HOME/auth.json` with mode 0600 before the adapter
    starts. `NO_BROWSER=1` so the adapter never advertises browser login.
    `CODEX_CONFIG={"model":"<model>"}` when a model is set.
    `INITIAL_AGENT_MODE=agent-full-access`: the Substrate sandbox is the
    isolation boundary; Codex's own sandbox would only get in the way.
- **ACP client** (`github.com/coder/acp-go-sdk`, v0.13.5):
  `Initialize` with `Fs{ReadTextFile, WriteTextFile}` and `Terminal: false`
  (both adapters run shell commands themselves when the client offers no
  terminal). Then `NewSession{Cwd}`, then `Prompt{TextBlock(prompt)}`. The
  `Prompt` call returns with the stop reason when the turn ends.
- **`acp.Client` implementation.**
  - `RequestPermission`: pick the first `allow_always` option, else
    `allow_once`, else the first option. Nobody is at the keyboard.
  - `ReadTextFile` / `WriteTextFile`: plain file I/O, paths must be absolute
    and under `Cwd` (the same confinement the Antigravity bootstrap applies).
  - `SessionUpdate`: write agent message text, tool-call titles and status,
    and plan entries to `Output` as plain lines. Thought chunks are dropped.
  - Terminal methods: return "not supported" errors; the capability is not
    advertised so they are never called.
  - `auth_required` from `NewSession`/`Prompt` is returned as an error that
    names the env var/secret that was expected; `Authenticate` is never
    called (both adapters' auth methods are interactive logins).
- **Timeout.** `Run` has none of its own; callers pass a deadline on `ctx`.

**Workspace goal** (`internal/workspace/setup.go`, `runBootstrap`): if
`agent.FromEnv()` reports an agent, call `agent.Run` with the same timeout
(`AX_BOOTSTRAP_TIMEOUT`, default 10m), `Cwd` = workspace path, and a prompt of
the form "Set up the workspace directory <path> (your current working
directory) according to the following goal: <goal>. Only create or modify
files inside it." Otherwise fall through to the existing Antigravity script.
Non-`end_turn` stop reasons and errors are logged and setup continues, as
today. `SetupResult.BootstrapRan` reflects the agent run the same way.

**Task body** (`runner/runner.go`, `Run`): after workspace setup, if
`task.spec.agent != nil`, call `agent.Run` with `Cwd` = workspace path,
`Prompt` = `spec.agent.prompt`, `Env` = `commandEnv(...)`, `Output` =
stdout, instead of `exec`-ing `spec.command`. Report through `OnCommandExit`
with `ExitCode` 0 for `end_turn`, 1 otherwise, `Err` set when `Run` erred.
Then block on `ctx.Done()` exactly as the command path does. If
`spec.agent` is set but `AX_AGENT` is not in the environment (control plane
older than the runner, or no ACP Model), log an error and serve metadata until
stopped, mirroring the "no task command specified" path.

Local runs: `ax-task-runner --task-file` keeps working; `AX_AGENT` and the
credential come from the shell environment.

### Image (`Dockerfile.task-runner`)

Add Node 22 (NodeSource deb, pinned major) and
`npm install -g @agentclientprotocol/claude-agent-acp@<pinned>
@agentclientprotocol/codex-acp@<pinned>`. Keep Python, Antigravity, and the
bootstrap script. `@openai/codex` resolves its platform binary from
`optionalDependencies`; the image is built `--platform linux/amd64`, so no
extra handling. Add a `make proto` target for regeneration while there.

### Docs

- `docs/manifests.md`: `spec.agent`, the two new providers, what goes in the
  Secret and how to produce it (`claude setup-token`, `codex login`).
- `docs/sandbox.md` / `docs/runner.md`: the agent path next to the command
  path; `AX_AGENT`, `AX_AGENT_MODEL`, the two credential variables.
- `README.md` "Why?" table: one row for `spec.agent`.

## Risks and known limits

- **Credential changes reach new tasks only.** Substrate's `EnsureActor`
  returns an existing actor as-is, and a Task spec is immutable after
  creation, so a rotated Secret or a changed Model provisions a new
  ActorTemplate but never rebinds a live or suspended actor. Those tasks
  must be deleted and recreated. Same as `GEMINI_API_KEY` upstream; rebinding
  actors is a Substrate lifecycle change and out of scope.
- **Codex token rotation.** Codex refreshes `auth.json` as it runs. Every
  sandbox starts from the copy in the Secret; if OpenAI rotates refresh
  tokens on use, a sandbox's refresh can invalidate the Secret's copy for the
  next one. Not handled. Mitigation is operational: re-run `codex login` and
  update the Secret when tasks start failing with `auth_required`. The Claude
  token from `claude setup-token` is long-lived and not rotated by use.
- **Subscription rate limits.** One seat's quota is shared by every task in
  the atespace. Fan-out hits it fast. Out of scope; AX already has no budget
  guardrails (roadmap item).
- **Terms of service.** Driving a consumer subscription from an automated
  cluster workload may be outside both vendors' terms. The user owns that
  call.
- **Credentials in the ActorTemplate.** Same posture as `GEMINI_API_KEY`
  upstream: the value is in the template spec and in the container
  environment. Not worse than today, not better.
- **A Model names the Secret.** Unlike the fixed `gemini-api-secret` name
  upstream, `Model.spec.secretKey` can point at any Secret in the atespace,
  and the control plane would copy it into a task. To keep a Model from
  becoming a Secret reader, only Secrets labeled `ax.io/agent-credential=true`
  are eligible (`model.GetAgentCredential`); anything else is
  `CredentialNotFound`. Added during implementation after a security review
  of the first controller commit.
- **Adapter churn.** Both adapters publish several times a week and the ACP
  spec is at v1/v2 transition. Versions are pinned in the Dockerfile; the Go
  SDK is pinned in `go.mod`. `acp-go-sdk` v0.13.5 speaks protocol v1, and
  both adapters still serve v1 (`CLAUDE_AGENT_ACP_PROTOCOL=auto`).

## Left to the builder

- Exact adapter and Node versions to pin (latest at build time).
- How `ax describe task` renders `spec.agent` (minimum: show the prompt, like
  it shows the command).
- Whether `Model.spec.provider` is validated at apply time against a known
  list, or left free-form as it is upstream.
- Test shape for `internal/agent`: an in-process fake ACP agent over
  `io.Pipe` using the SDK's `NewAgentSideConnection` is the obvious one and
  needs no network or credentials. Controller tests extend the existing
  fake-store / fake-secret pattern.
- A local smoke test of the runner against the real `claude-agent-acp` and
  the real `codex-acp` with the developer's own logins, before any image is
  pushed. The per-kind environment contract above (`CODEX_CONFIG`,
  `INITIAL_AGENT_MODE`, `NO_BROWSER`, `ANTHROPIC_MODEL`) is read from the
  adapters' READMEs and must be confirmed against the pinned adapter source;
  fallbacks are `session/set_mode` from `NewSessionResponse.Modes` and a
  `config.toml` in `CODEX_HOME`.

## Decisions made here

- Agent choice lives in `Model`, not in `Task.spec.agent.kind`. The Model
  already exists for "which provider, which credential"; putting the kind on
  the Task would duplicate it and leave the credential nowhere to go.
- `default-model` is the fallback so a Workspace `goal` picks up the atespace's
  agent without every Task naming one.
- Credentials travel as environment variables, not mounted Secrets. AX has no
  volume plumbing today; adding it for this is a bigger change than the
  feature.
- `Terminal: false`. Offering an ACP terminal would make the runner the
  process supervisor for the agent's shell commands; the adapters already do
  that well themselves.
- Permissions auto-approved. There is no one to ask. The sandbox is the
  boundary.
- Rotated credentials do not reach existing tasks. Making them would mean
  rebinding or recreating Substrate actors (and losing their snapshots); the
  feature inherits upstream's behavior and documents it instead.
- A missing Secret only fails tasks that asked for an agent. Plain command
  tasks in an atespace with an ACP `default-model` keep running, credential
  or not, so adding a Model never breaks tasks that do not use it.
