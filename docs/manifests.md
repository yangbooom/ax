# Writing manifests

All four kinds can live in one multi-document YAML file. See [`examples/task.yaml`](../examples/task.yaml) for a complete, working set.

`metadata.name` and `metadata.atespace` become Substrate resource names, so they must be lowercase RFC 1123 labels: at most 63 lowercase alphanumeric characters or `-`, starting and ending with an alphanumeric character. `ax apply` rejects anything else up front rather than letting the task fail later with `ActorCreationFailed`.

When `metadata.atespace` is omitted, `ax apply -a team-a -f manifest.yaml` uses `team-a`. Without `-a` or `--atespace`, the manifest's atespace is preserved, or defaults to `default` if omitted. If an explicit flag disagrees with `metadata.atespace`, that resource is rejected before it is submitted.

## Task

```yaml
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: task123
  atespace: default
spec:
  image: "ghcr.io/my-org/my-agent-image"
  command: ["python", "agent.py"]   # or `agent: {prompt: "..."}` to run Claude Code / Codex instead, see below
  env:
    - name: ENVIRONMENT
      value: "production"

  resources:
    limits:
      cpu: "2"
      memory: "4Gi"

  workspace:
    name: default-workspace
    path: "/workspace"   # optional; this is the default
    goal: "Install dependencies and run the test suite"   # the atespace's agent prepares the workspace to this goal on first run

  debug: true   # serve guest services inside the sandbox so `ax ssh` works; off by default
```

### Image references

Substrate requires `spec.image` to be pinned by digest, e.g. `ghcr.io/my-org/my-agent-image@sha256:...`. A tag-only reference such as `my-agent:v1` is rejected when the task's ActorTemplate is created, because a tag can move and would silently invalidate the actor's snapshots. If that happens the task reports `Ready=False` with reason `TemplateCreationFailed` and the Substrate error as the message. Resolve a tag to its digest with `docker buildx imagetools inspect <image:tag>` or `crane digest <image:tag>`.

### Binding a workspace

A task binds at most one `Workspace` through `spec.workspace`. The binding needs a `name`; without a `path` the workspace is mounted at `/workspace`. That path is the working directory of `spec.command`, and the task reports `WorkspaceReady` once the workspace is prepared. A task with no `workspace` gets an empty working directory at `/workspace`.

To work on content from several sources, put it all in one `Workspace`, for example by listing several repositories under `spec.git` or adding files through `spec.files`.

### Running an agent instead of a command

`spec.agent` hands the task to a coding agent instead of running a command. The runner starts Claude Code or Codex over the [Agent Client Protocol](https://agentclientprotocol.com), opens one session in the workspace, sends `prompt` as a single turn, and streams what the agent does to the task log. The sandbox stays up afterwards, so `ax ssh` can inspect the result.

```yaml
spec:
  agent:
    model: default-model        # optional; the Model whose provider runs the task
    prompt: "Find the flaky test in ./internal/lock and fix it. Commit on a branch."
  workspace:
    name: my-repo
    goal: "Install the toolchain"   # the same agent prepares the workspace first
```

`agent` and `command` are mutually exclusive, and `prompt` is required. Which agent runs, with which vendor model and credential, comes from the `Model` named by `agent.model`, or `default-model` in the task's atespace when it is left out. The Model's `provider` must be `claude-code` or `codex`; a task whose Model is missing, has another provider, or has no credential in its Secret is marked `Failed` with reason `ModelNotFound`, `UnsupportedProvider`, or `CredentialNotFound`. See [Claude Code and Codex](#claude-code-and-codex-subscription) below for the Model and Secret.

A Workspace `goal` on a task without `spec.agent` also runs on the atespace's `default-model` agent when that Model is an ACP provider with a credential; otherwise it falls back to Antigravity with `GEMINI_API_KEY`, as before.

### Sizing the sandbox

`spec.resources.limits` caps the CPU and memory of the task's sandbox. The controller copies the limits onto the Substrate `ActorTemplate` it provisions for the task, using Kubernetes quantity syntax (`500m`, `2`, `4Gi`). Only `cpu` and `memory` are supported, each quantity must be greater than zero, and the CPU limit must be below 1000 cores. `ax apply` rejects values outside these rules, and a task whose limits Substrate refuses is marked `Failed` with reason `TemplateCreationFailed` rather than run without them. A task without limits is sized by its worker's defaults.

Substrate sizes sandboxes by limits alone, so `spec.resources.requests` is not supported and `ax apply` rejects a manifest that sets it.

## Workspace

```yaml
apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: default-workspace
  atespace: default
spec:
  git:
    - name: origin
      repo: "https://github.com/chalk/chalk.git"
      branch: "main"
  files:
    - path: "AGENTS.md"
      content: |
        # Project Guidelines
        - Run `go test ./...` before submitting changes.
        - Keep dependencies minimal.
  mcp:
    registries:
      - provider: google
        query: "mcp.tags:build"
    servers:
      - name: git-tools
        endpoint: "http://git-mcp.default.svc.cluster.local:8080"
  skills:
    registries:
      - provider: google
        query: "skills.tags:nodejs"
    path: "/.agents/skills"
```


## Model

For Google models, store the API key and set `provider: google`.

```bash
kubectl create secret generic gemini-api-secret --from-literal=GEMINI_API_KEY="AIzaSy..."
```

Then reference it from the `Model`:

```yaml
apiVersion: ax.io/v1alpha1
kind: Model
metadata:
  name: default-model
  atespace: default
spec:
  provider: google
  model: gemini-3.8-flash
  secretKey:
    name: gemini-api-secret
    key: GEMINI_API_KEY
```

### Claude Code and Codex (subscription)

Two more providers run a coding agent on a subscription you already pay for, instead of an API key. They are what `spec.agent` and the goal-based workspace setup use. The Secret holds the agent's own login, produced once on your machine:

| `provider` | Agent | Secret value |
|---|---|---|
| `claude-code` | Claude Code | the token printed by `claude setup-token` |
| `codex` | Codex | the contents of `$CODEX_HOME/auth.json` (default `~/.codex/auth.json`) after `codex login` |

```bash
kubectl create secret generic claude-subscription --from-literal=CLAUDE_CODE_OAUTH_TOKEN="$(claude setup-token)"
kubectl create secret generic codex-subscription --from-file=auth.json="$HOME/.codex/auth.json"
kubectl label secret claude-subscription codex-subscription ax.io/agent-credential=true
```

The label is required. A Model can name any Secret in its atespace, and the control plane copies the value into task containers, so only Secrets explicitly marked `ax.io/agent-credential=true` are eligible; a Model pointing at an unlabeled Secret fails its tasks with `CredentialNotFound`.

```yaml
apiVersion: ax.io/v1alpha1
kind: Model
metadata:
  name: default-model
  atespace: default
spec:
  provider: claude-code
  model: claude-opus-5-5          # optional vendor model id; empty lets the agent choose
  secretKey:
    name: claude-subscription
    key: CLAUDE_CODE_OAUTH_TOKEN
---
apiVersion: ax.io/v1alpha1
kind: Model
metadata:
  name: codex
  atespace: default
spec:
  provider: codex
  model: gpt-5.5-codex
  secretKey:
    name: codex-subscription
    key: auth.json
```

Name the Model `default-model` to make it the atespace's default: tasks whose `spec.agent` leaves `model` empty, and Workspace goals on command tasks, use it. `parameters` is ignored for these providers.

Two things to know. The credential reaches a task through its ActorTemplate environment when the task is created; rotating the Secret or changing the Model affects tasks created afterwards, and existing ones must be recreated. And Codex refreshes `auth.json` as it runs, so if tasks start failing with "Authentication required", run `codex login` again and update the Secret. A full example lives in [`examples/agent-task.yaml`](../examples/agent-task.yaml).
