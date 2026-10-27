# ACP Agents Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a `Task` run Claude Code or Codex (over ACP, on subscription credentials) as its body and as its workspace `goal` setup, instead of a shell command and the Gemini-backed Antigravity bootstrap.

**Architecture:** A new `internal/agent` package is a Go ACP client that spawns an adapter process (`claude-agent-acp` / `codex-acp`) over stdio, opens one session in the workspace, sends one prompt, and returns the stop reason. The control plane picks the agent kind and credential from a `Model` resource and ships them to the container as environment variables on the per-task ActorTemplate, the same way `GEMINI_API_KEY` travels today. The runner reads those variables for both the goal bootstrap and the task body.

**Tech Stack:** Go 1.27, `github.com/coder/acp-go-sdk` v0.13.5, protobuf (`protoc` + `protoc-gen-go` v1.36.12 + `protoc-gen-go-grpc` v1.6.0), Node 22 + npm adapters in `Dockerfile.task-runner`.

**Spec:** `docs/superpowers/specs/2026-10-27-acp-agents-design.md`

## Global Constraints

- Go module `github.com/google/ax`, Go 1.27.1; one new dependency only: `github.com/coder/acp-go-sdk v0.13.5`.
- API types are protobuf-generated. Edit `pkg/apis/v1alpha1/ax.proto`, regenerate; never hand-edit `*.pb.go`.
- Every Go file starts with the repository's Apache-2.0 header (copy it from any sibling file).
- Env var names, exact: `AX_AGENT`, `AX_AGENT_MODEL`, `AX_AGENT_COMMAND`, `CLAUDE_CODE_OAUTH_TOKEN`, `CODEX_AUTH_JSON`.
- Provider strings, exact: `claude-code`, `codex`. Adapter binaries, exact: `claude-agent-acp`, `codex-acp`.
- Condition reasons, exact: `ModelNotFound`, `UnsupportedProvider`, `CredentialNotFound`.
- Codex home inside the container: `/ax/codex` (under `workspace.AXDir`); Claude fallback home: `/ax/home`.
- Bootstrap timeout stays `AX_BOOTSTRAP_TIMEOUT`, default 10m, read by the existing `bootstrapTimeout()`.
- A Task without `spec.agent` in an atespace without an ACP `Model` must behave exactly as upstream. Run `go test ./...` after every task; nothing that passed before may fail.
- Commit messages: imperative, scoped like upstream (`api:`, `runner:`, `controller:`, `agent:`, `docs:`), no attribution lines of any kind.

## Review Focus

1. A prompt that makes the agent write outside the workspace through the ACP `fs/write_text_file` path must be refused, not written. (Task 3: `TestClient_WriteTextFile_OutsideCwdRefused`)
2. An adapter that is on the system but dies before answering `initialize` (bad credential file, missing node) must surface as an error from `Run`, not a hang. (Task 3: `TestRun_AdapterExitsBeforeInitialize`)
3. `SIGTERM` to the runner while the agent is mid-turn must take the adapter down with it, process group included. (Task 3: `TestRun_CancelKillsAdapterProcessGroup`)
4. A `spec.agent` task whose `default-model` is a `google` Model must fail at reconcile with `UnsupportedProvider`, never reach the runner. (Task 5: `TestTaskReconciler_AgentUnsupportedProvider`)
5. `spec.agent` and `spec.command` both set must be rejected at `ax apply`, before anything is stored. (Task 2: `TestValidateTask_AgentAndCommandExclusive`)

---

## File structure

| Path | Responsibility |
|---|---|
| `pkg/apis/v1alpha1/ax.proto`, `ax.pb.go` | `AgentSpec` message, `TaskSpec.agent` field |
| `pkg/apis/v1alpha1/types.go` | provider constants, `IsACPProvider`, `ValidateTask` rules for `spec.agent` |
| `internal/agent/env.go` | env var names, `Kind`, `FromEnv`, per-kind adapter command and environment |
| `internal/agent/client.go` | the `acp.Client` implementation (permissions, fs, update streaming) |
| `internal/agent/run.go` | `Spec`, `Run`: spawn adapter, drive one turn, tear down |
| `internal/agent/fakeagent/main.go` | a tiny ACP agent binary used by tests in three packages |
| `internal/agent/agenttest/build.go` | `Build(t)` compiles `fakeagent` once per test binary |
| `internal/workspace/setup.go` | `runBootstrap` dispatches to `agent.Run` when configured |
| `runner/runner.go` | agent task body next to the command path |
| `internal/controller/reconciler.go` | `ModelResolver`, agent env injection, the three failure reasons |
| `cmd/ax-server/main.go` | wires `ModelResolver` to the store |
| `cmd/ax/main.go` | `ax describe task` prints `Prompt:` |
| `Dockerfile.task-runner`, `Makefile` | Node + adapters, `make proto` |
| `docs/manifests.md`, `docs/sandbox.md`, `docs/runner.md`, `README.md`, `examples/agent-task.yaml` | user-facing docs |

---

### Task 1: API — `AgentSpec` and `make proto`

**Files:**
- Modify: `pkg/apis/v1alpha1/ax.proto:70-83` (TaskSpec), add `AgentSpec` after `WorkspaceRef`
- Modify: `Makefile` (new `proto` target)
- Regenerate: `pkg/apis/v1alpha1/ax.pb.go`, `pkg/apis/v1alpha1/ax_grpc.pb.go`
- Test: `pkg/apis/v1alpha1/types_test.go`

**Interfaces:**
- Produces: `TaskSpec.Agent *AgentSpec` with `GetAgent()`; `AgentSpec{Model string; Prompt string}` with `GetModel()`, `GetPrompt()`. YAML keys `agent`, `model`, `prompt`.

- [ ] **Step 1: Write the failing round-trip test**

In `pkg/apis/v1alpha1/types_test.go`, next to the existing YAML round-trip tests:

```go
func TestTaskYAML_AgentRoundTrip(t *testing.T) {
	in := "apiVersion: ax.io/v1alpha1\nkind: Task\nmetadata:\n  name: t\nspec:\n  agent:\n    model: m\n    prompt: do it\n"
	var task Task
	if err := yaml.Unmarshal([]byte(in), &task); err != nil {
		t.Fatal(err)
	}
	if got := task.GetSpec().GetAgent(); got.GetModel() != "m" || got.GetPrompt() != "do it" {
		t.Fatalf("agent = %+v", got)
	}
	out, err := yaml.Marshal(&task)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "prompt: do it") {
		t.Fatalf("marshal lost agent: %s", out)
	}
}
```

(Use whatever unmarshal helper the sibling tests use if they do not call `yaml.Unmarshal` directly.)

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./pkg/apis/v1alpha1/ -run TestTaskYAML_AgentRoundTrip`
Expected: compile error, `task.GetSpec().GetAgent undefined`.

- [ ] **Step 3: Add the proto messages**

In `ax.proto`, inside `TaskSpec` after `bool debug = 6;`:

```proto
  // agent runs an ACP agent (Claude Code or Codex) with a prompt as the task
  // body instead of spec.command. Exactly one of command and agent may be set.
  AgentSpec agent = 7;
```

After `message WorkspaceRef { ... }`:

```proto
message AgentSpec {
  // model names the Model resource in the task's atespace whose provider runs
  // this task. Empty means "default-model".
  string model = 1;
  // prompt is the single user turn sent to the agent, run with the workspace
  // path as its working directory.
  string prompt = 2;
}
```

- [ ] **Step 4: Add `make proto` and regenerate**

Append to `Makefile`:

```make
# Regenerate pkg/apis/v1alpha1/*.pb.go from ax.proto. Plugin versions are
# pinned so the generated headers stay stable between contributors.
proto:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.0
	PATH="$$(go env GOPATH)/bin:$$PATH" protoc -I . \
	  --go_out=. --go_opt=paths=source_relative \
	  --go-grpc_out=. --go-grpc_opt=paths=source_relative \
	  pkg/apis/v1alpha1/ax.proto
```

Run `make proto`. Add `proto` to the existing `.PHONY` list.

- [ ] **Step 5: Run the test and the whole suite**

Run: `go test ./pkg/apis/v1alpha1/ -run TestTaskYAML_AgentRoundTrip` → PASS.
Run: `go build ./... && go test ./...` → all PASS. `git diff --stat pkg/apis/v1alpha1/` should show only `ax.proto`, `ax.pb.go` (and a version-header-only change in `ax_grpc.pb.go`, if any).

- [ ] **Step 6: Commit**

```bash
git add pkg/apis/v1alpha1/ax.proto pkg/apis/v1alpha1/ax.pb.go pkg/apis/v1alpha1/ax_grpc.pb.go pkg/apis/v1alpha1/types_test.go Makefile
git commit -m "api: add Task spec.agent for ACP agent tasks"
```

---

### Task 2: API — provider constants and validation

**Files:**
- Modify: `pkg/apis/v1alpha1/types.go:309-323` (`ValidateTask`), add constants near the other `Default*`/`Kind*` constants
- Test: `pkg/apis/v1alpha1/types_test.go`

**Interfaces:**
- Produces:
  ```go
  const (
      ProviderGoogle     = "google"
      ProviderClaudeCode = "claude-code"
      ProviderCodex      = "codex"
  )
  // IsACPProvider reports whether provider names an agent AX drives over ACP.
  func IsACPProvider(provider string) bool
  ```
  `ValidateTask` rejects `spec.agent` + `spec.command` together and `spec.agent` with an empty prompt.

- [ ] **Step 1: Write the failing tests**

```go
func TestValidateTask_AgentAndCommandExclusive(t *testing.T) {
	task := &Task{Metadata: &ObjectMeta{Name: "t"}, Spec: &TaskSpec{
		Command: []string{"sh"}, Agent: &AgentSpec{Prompt: "p"},
	}}
	if err := ValidateTask(task); err == nil || !strings.Contains(err.Error(), "spec.agent") {
		t.Fatalf("want spec.agent/command error, got %v", err)
	}
}

func TestValidateTask_AgentNeedsPrompt(t *testing.T) {
	task := &Task{Metadata: &ObjectMeta{Name: "t"}, Spec: &TaskSpec{Agent: &AgentSpec{}}}
	if err := ValidateTask(task); err == nil || !strings.Contains(err.Error(), "prompt") {
		t.Fatalf("want prompt error, got %v", err)
	}
	task.Spec.Agent.Prompt = "p"
	if err := ValidateTask(task); err != nil {
		t.Fatalf("valid agent task rejected: %v", err)
	}
}

func TestIsACPProvider(t *testing.T) {
	for p, want := range map[string]bool{"claude-code": true, "codex": true, "google": false, "": false, "Codex": false} {
		if got := IsACPProvider(p); got != want {
			t.Errorf("IsACPProvider(%q) = %v, want %v", p, got, want)
		}
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./pkg/apis/v1alpha1/ -run 'TestValidateTask_Agent|TestIsACPProvider'`
Expected: compile error on `IsACPProvider`; after stubbing, the validation tests FAIL with `got <nil>`.

- [ ] **Step 3: Implement**

Add the constants and `IsACPProvider` to `types.go`. In `ValidateTask`, before the `ValidateResources` return:
- both set → `errors.New("spec.agent and spec.command are mutually exclusive")`
- agent set, prompt empty → `errors.New("spec.agent: prompt is required")`

- [ ] **Step 4: Run tests**

Run: `go test ./pkg/apis/v1alpha1/` → PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/apis/v1alpha1/types.go pkg/apis/v1alpha1/types_test.go
git commit -m "api: validate spec.agent and name the ACP providers"
```

---

### Task 3: `internal/agent` — ACP client, fake agent, `Run`

This is the largest task; its five sub-steps each end with a passing test.

**Files:**
- Create: `internal/agent/env.go`, `internal/agent/client.go`, `internal/agent/run.go`
- Create: `internal/agent/fakeagent/main.go`, `internal/agent/agenttest/build.go`
- Test: `internal/agent/client_test.go`, `internal/agent/run_test.go`
- Modify: `go.mod`, `go.sum` (`go get github.com/coder/acp-go-sdk@v0.13.5`)

**Interfaces:**
- Produces (package `agent`):
  ```go
  type Kind string
  const (
      KindClaudeCode Kind = v1alpha1.ProviderClaudeCode
      KindCodex      Kind = v1alpha1.ProviderCodex
  )
  const (
      EnvKind        = "AX_AGENT"
      EnvModel       = "AX_AGENT_MODEL"
      EnvCommand     = "AX_AGENT_COMMAND"       // dev/test override: adapter argv, space-separated
      EnvClaudeToken = "CLAUDE_CODE_OAUTH_TOKEN"
      EnvCodexAuth   = "CODEX_AUTH_JSON"
  )
  // FromEnv reports the agent configured for this container, if any.
  func FromEnv() (kind Kind, model string, ok bool)

  // StateDir holds adapter state outside the workspace: <StateDir>/codex is
  // CODEX_HOME and <StateDir>/home the HOME fallback. Same default as
  // workspace.DefaultAXDir; tests override it. (agent cannot import workspace:
  // workspace imports agent from Task 4 on.)
  var StateDir = "/ax"

  type Spec struct {
      Kind   Kind
      Model  string
      Cwd    string
      Prompt string
      Env    []string  // base environment for the adapter; per-kind vars are appended
      Output io.Writer // nil means io.Discard
  }
  // Run drives one prompt through the adapter and returns its stop reason.
  func Run(ctx context.Context, s Spec) (acp.StopReason, error)
  ```
- Produces (package `agenttest`): `func Build(t testing.TB) string` → path of a built `fakeagent` binary, cached per test binary with `sync.Once`.
- `fakeagent` behavior is driven by env vars, all prefixed `FAKEAGENT_`:
  - `FAKEAGENT_MODE`: `echo` (default: reply with `echo: <prompt>` as one agent message chunk, then a tool call `tc-1` titled `fake tool` that transitions to `completed`, stop `end_turn`), `auth-required` (`session/new` returns `acp.NewAuthRequired(nil)`), `permission` (asks permission with options `[reject_once, allow_once, allow_always]`, replies with the chosen option id as a message chunk `chose: <id>`), `write` (calls `fs/write_text_file` on `FAKEAGENT_WRITE_PATH` with content `written`; on error replies `write error: <msg>`), `hang` (never answers `session/prompt`; used for cancel tests), `die` (exits 3 before answering `initialize`).
  - It always first replies with one message chunk `env: CWD=<cwd> HOME=<$HOME> CODEX_HOME=<$CODEX_HOME> NO_BROWSER=<$NO_BROWSER> ANTHROPIC_MODEL=<$ANTHROPIC_MODEL> CODEX_CONFIG=<$CODEX_CONFIG> INITIAL_AGENT_MODE=<$INITIAL_AGENT_MODE>` so tests can assert the per-kind environment. `<cwd>` is `os.Getwd()`; the agent `chdir`s to the session `cwd` on `session/new`.
  - In `hang` mode it writes its pid to `FAKEAGENT_PID_FILE` and spawns `sleep 300` as a child so process-group kill can be verified.

#### 3a. Dependency, env, and per-kind environment

- [ ] **Step 1: Add the dependency**

Run: `go get github.com/coder/acp-go-sdk@v0.13.5 && go mod tidy`. Confirm `go.mod` gained exactly that one `require` line.

- [ ] **Step 2: Write failing tests for `FromEnv` and the per-kind environment**

`internal/agent/run_test.go` (package `agent`, internal test so it can call `adapterEnv`):

```go
func TestFromEnv(t *testing.T) {
	t.Setenv(EnvKind, "")
	if _, _, ok := FromEnv(); ok {
		t.Fatal("unset AX_AGENT must report ok=false")
	}
	t.Setenv(EnvKind, "codex")
	t.Setenv(EnvModel, "gpt-5.5-codex")
	if k, m, ok := FromEnv(); !ok || k != KindCodex || m != "gpt-5.5-codex" {
		t.Fatalf("got %q %q %v", k, m, ok)
	}
	t.Setenv(EnvKind, "gemini")
	if _, _, ok := FromEnv(); ok {
		t.Fatal("unknown kind must report ok=false")
	}
}

func TestAdapterEnv_Codex(t *testing.T) {
	home := t.TempDir()
	t.Setenv(EnvCodexAuth, `{"tokens":{}}`)
	env, err := adapterEnv(Spec{Kind: KindCodex, Model: "m"}, home)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"CODEX_HOME": home, "NO_BROWSER": "1", "CODEX_CONFIG": `{"model":"m"}`, "INITIAL_AGENT_MODE": "agent-full-access"}
	for k, v := range want {
		if got := lookupEnv(env, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	data, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil || string(data) != `{"tokens":{}}` {
		t.Fatalf("auth.json = %q, %v", data, err)
	}
	if fi, _ := os.Stat(filepath.Join(home, "auth.json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("auth.json mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestAdapterEnv_ClaudeCode(t *testing.T) {
	home := t.TempDir()
	env, err := adapterEnv(Spec{Kind: KindClaudeCode, Model: "claude-opus-5-5", Env: []string{"HOME=/nonexistent/ro"}}, home)
	if err != nil {
		t.Fatal(err)
	}
	if got := lookupEnv(env, "ANTHROPIC_MODEL"); got != "claude-opus-5-5" {
		t.Errorf("ANTHROPIC_MODEL = %q", got)
	}
	if got := lookupEnv(env, "HOME"); got != home {
		t.Errorf("HOME = %q, want fallback %q", got, home)
	}
}
```

`lookupEnv(env []string, key string) string` is a test helper (last match wins, like the OS).

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./internal/agent/ -run 'TestFromEnv|TestAdapterEnv'` → compile errors.

- [ ] **Step 4: Implement `env.go`**

```go
func FromEnv() (Kind, string, bool)
// adapterCommand returns the adapter argv for k, honoring EnvCommand when set.
func adapterCommand(k Kind) []string
// adapterEnv returns s.Env plus the per-kind variables. stateDir is the
// directory used for CODEX_HOME (codex) or the HOME fallback (claude-code);
// callers pass filepath.Join(workspace.AXDir, "codex"|"home").
func adapterEnv(s Spec, stateDir string) ([]string, error)
```

`adapterEnv` rules, from the spec: codex → mkdir `stateDir` 0700, write `$CODEX_AUTH_JSON` to `stateDir/auth.json` 0600 when the env var is non-empty, append `CODEX_HOME`, `NO_BROWSER=1`, `INITIAL_AGENT_MODE=agent-full-access`, and `CODEX_CONFIG={"model":"<m>"}` (via `json.Marshal`) when `s.Model != ""`. claude-code → append `ANTHROPIC_MODEL` when set; if `HOME` in `s.Env` is empty or not a writable directory (`os.MkdirAll` + a probe `os.CreateTemp` fails), mkdir `stateDir` and append `HOME=<stateDir>`. Unknown kind → error.

- [ ] **Step 5: Run tests** → PASS.

#### 3b. The `acp.Client` implementation

- [ ] **Step 6: Write failing tests for the client (no process, direct calls)**

`internal/agent/client_test.go`:

```go
func TestClient_RequestPermission_PrefersAllowAlways(t *testing.T) {
	c := newClient(t.TempDir(), io.Discard)
	opts := []acp.PermissionOption{
		{OptionId: "r", Kind: acp.PermissionOptionKindRejectOnce},
		{OptionId: "o", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionId: "a", Kind: acp.PermissionOptionKindAllowAlways},
	}
	resp, err := c.RequestPermission(context.Background(), acp.RequestPermissionRequest{Options: opts})
	if err != nil || resp.Outcome.Selected == nil || resp.Outcome.Selected.OptionId != "a" {
		t.Fatalf("got %+v, %v", resp, err)
	}
	resp, _ = c.RequestPermission(context.Background(), acp.RequestPermissionRequest{Options: opts[:2]})
	if resp.Outcome.Selected.OptionId != "o" {
		t.Fatalf("want allow_once, got %+v", resp)
	}
	resp, _ = c.RequestPermission(context.Background(), acp.RequestPermissionRequest{Options: opts[:1]})
	if resp.Outcome.Selected.OptionId != "r" {
		t.Fatalf("want first option, got %+v", resp)
	}
	resp, _ = c.RequestPermission(context.Background(), acp.RequestPermissionRequest{})
	if resp.Outcome.Cancelled == nil {
		t.Fatalf("no options must cancel, got %+v", resp)
	}
}

func TestClient_WriteTextFile_OutsideCwdRefused(t *testing.T) {
	cwd := t.TempDir()
	c := newClient(cwd, io.Discard)
	outside := filepath.Join(t.TempDir(), "x.txt")
	if _, err := c.WriteTextFile(context.Background(), acp.WriteTextFileRequest{Path: outside, Content: "no"}); err == nil {
		t.Fatal("write outside cwd must fail")
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("file outside cwd was written")
	}
	if _, err := c.WriteTextFile(context.Background(), acp.WriteTextFileRequest{Path: "relative.txt", Content: "no"}); err == nil {
		t.Fatal("relative path must fail")
	}
	inside := filepath.Join(cwd, "sub", "ok.txt")
	if _, err := c.WriteTextFile(context.Background(), acp.WriteTextFileRequest{Path: inside, Content: "yes"}); err != nil {
		t.Fatal(err)
	}
	got, _ := c.ReadTextFile(context.Background(), acp.ReadTextFileRequest{Path: inside})
	if got.Content != "yes" {
		t.Fatalf("read back %q", got.Content)
	}
}

func TestClient_SessionUpdate_StreamsText(t *testing.T) {
	var buf bytes.Buffer
	c := newClient(t.TempDir(), &buf)
	ctx := context.Background()
	_ = c.SessionUpdate(ctx, acp.SessionNotification{Update: acp.UpdateAgentMessageText("hello")})
	_ = c.SessionUpdate(ctx, acp.SessionNotification{Update: acp.StartToolCall("tc", "run tests")})
	_ = c.SessionUpdate(ctx, acp.SessionNotification{Update: acp.UpdateAgentThoughtText("secret thought")})
	out := buf.String()
	if !strings.Contains(out, "hello") || !strings.Contains(out, "run tests") {
		t.Fatalf("missing text/tool call in %q", out)
	}
	if strings.Contains(out, "secret thought") {
		t.Fatalf("thought chunk leaked: %q", out)
	}
}
```

- [ ] **Step 7: Run to verify they fail** → compile error on `newClient`.

- [ ] **Step 8: Implement `client.go`**

```go
type client struct{ cwd string; out io.Writer }
func newClient(cwd string, out io.Writer) *client
var _ acp.Client = (*client)(nil)
```

Path confinement: `filepath.IsAbs(p)` and `filepath.Rel(cwd, filepath.Clean(p))` does not start with `..`. `SessionUpdate` writes `AgentMessageChunk` text verbatim, `ToolCall` as `[tool] <title> (<status>)`, `ToolCallUpdate` as `[tool] <id> <status>` when status is non-nil, `Plan` entries as `[plan] <status> <content>`; everything else is ignored. Terminal methods return `errors.New("terminal not supported")`.

- [ ] **Step 9: Run tests** → PASS.

#### 3c. The fake agent and its builder

- [ ] **Step 10: Write `internal/agent/fakeagent/main.go` and `internal/agent/agenttest/build.go`**

`fakeagent` is a `main` package implementing `acp.Agent` per the Interfaces block, served with `acp.NewAgentSideConnection(agent, os.Stdout, os.Stdin)` and blocking on `conn.Done()`. Unused interface methods return `acp.NewMethodNotFound`-style errors (any `*acp.RequestError` is fine). `Initialize` returns `ProtocolVersion: acp.ProtocolVersionNumber`.

`agenttest.Build(t)`:

```go
// Build compiles the fakeagent binary once per test process and returns its
// path. Tests put it in AX_AGENT_COMMAND to stand in for a real adapter.
func Build(t testing.TB) string
```

Implementation: `sync.Once` around `go build -o <os.TempDir()/ax-fakeagent-<pid>> github.com/google/ax/internal/agent/fakeagent`, `t.Fatalf` on failure, `t.Cleanup` not needed (OS temp). Same technique as `cmd/ax/main_test.go:42`.

- [ ] **Step 11: Smoke it by hand**

Run: `go build -o /tmp/fa ./internal/agent/fakeagent && echo OK`. Expected: `OK`. (No test yet; 3d exercises it.)

#### 3d. `Run`

- [ ] **Step 12: Write the failing end-to-end tests**

Append to `internal/agent/run_test.go`:

```go
func fakeSpec(t *testing.T, mode string, extra ...string) Spec {
	t.Helper()
	t.Setenv(EnvCommand, agenttest.Build(t))
	t.Setenv("FAKEAGENT_MODE", mode)
	var buf bytes.Buffer
	return Spec{Kind: KindClaudeCode, Cwd: t.TempDir(), Prompt: "ping", Output: &buf,
		Env: append(os.Environ(), extra...)}
}

func TestRun_EchoesPromptAndEndsTurn(t *testing.T) {
	s := fakeSpec(t, "echo")
	reason, err := Run(context.Background(), s)
	if err != nil || reason != acp.StopReasonEndTurn {
		t.Fatalf("reason=%q err=%v", reason, err)
	}
	out := s.Output.(*bytes.Buffer).String()
	for _, want := range []string{"echo: ping", "fake tool", "CWD=" + mustEval(s.Cwd)} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRun_CodexEnvReachesAdapter(t *testing.T) {
	s := fakeSpec(t, "echo")
	s.Kind, s.Model = KindCodex, "gpt-5.5-codex"
	t.Setenv(EnvCodexAuth, `{"k":1}`)
	orig := StateDir
	StateDir = t.TempDir()
	t.Cleanup(func() { StateDir = orig })
	if _, err := Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	out := s.Output.(*bytes.Buffer).String()
	for _, want := range []string{"NO_BROWSER=1", `CODEX_CONFIG={"model":"gpt-5.5-codex"}`, "INITIAL_AGENT_MODE=agent-full-access", "CODEX_HOME=" + filepath.Join(StateDir, "codex")} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRun_AuthRequiredIsAnError(t *testing.T) {
	s := fakeSpec(t, "auth-required")
	_, err := Run(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), EnvClaudeToken) {
		t.Fatalf("want auth error naming %s, got %v", EnvClaudeToken, err)
	}
}

func TestRun_AutoApprovesPermission(t *testing.T) {
	s := fakeSpec(t, "permission")
	if _, err := Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if out := s.Output.(*bytes.Buffer).String(); !strings.Contains(out, "chose: allow_always") {
		t.Fatalf("permission not auto-approved:\n%s", out)
	}
}

func TestRun_FsWriteConfinedToCwd(t *testing.T) {
	s := fakeSpec(t, "write")
	outside := filepath.Join(t.TempDir(), "escape.txt")
	s.Env = append(s.Env, "FAKEAGENT_WRITE_PATH="+outside)
	if _, err := Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("agent wrote outside the workspace")
	}
	if out := s.Output.(*bytes.Buffer).String(); !strings.Contains(out, "write error") {
		t.Fatalf("expected write error surfaced:\n%s", out)
	}
}

func TestRun_AdapterExitsBeforeInitialize(t *testing.T) {
	s := fakeSpec(t, "die")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Run(ctx, s); err == nil {
		t.Fatal("want error when adapter dies before initialize")
	}
}

func TestRun_CancelKillsAdapterProcessGroup(t *testing.T) {
	s := fakeSpec(t, "hang")
	pidFile := filepath.Join(t.TempDir(), "pid")
	s.Env = append(s.Env, "FAKEAGENT_PID_FILE="+pidFile)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := Run(ctx, s); done <- err }()
	waitForFile(t, pidFile) // helper: poll up to 5s
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(readFile(t, pidFile)))
	// The whole group must be gone: signal 0 to -pid fails once no member is left.
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(-pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if syscall.Kill(-pid, 0) == nil {
		t.Fatal("adapter process group still alive after cancel")
	}
}
```

`mustEval` is `filepath.EvalSymlinks` with `t.Fatal` on error (macOS temp dirs are symlinked).

- [ ] **Step 13: Run to verify they fail** → compile error on `Run`.

- [ ] **Step 14: Implement `run.go`**

```go
func Run(ctx context.Context, s Spec) (acp.StopReason, error)
```

Sequence, from the spec: `adapterCommand(s.Kind)` → `exec.CommandContext`-free `exec.Command` (manual kill, see below) with `Dir: s.Cwd`, `Env: adapterEnv(...)`, `Stderr: os.Stderr`, `SysProcAttr{Setpgid: true}`, `StdinPipe`/`StdoutPipe`; start; `conn := acp.NewClientSideConnection(newClient(s.Cwd, out), stdin, stdout)`; a goroutine waits on `cmd.Wait()` and closes a `exited` channel. Each of `Initialize` (caps `Fs{ReadTextFile: true, WriteTextFile: true}`, `Terminal: false`), `NewSession{Cwd: s.Cwd, McpServers: []acp.McpServer{}}`, `Prompt{Prompt: []acp.ContentBlock{acp.TextBlock(s.Prompt)}}` is run through a helper that returns early with an error if `exited` or `conn.Done()` fires first (this is what makes `die` a quick failure instead of a hang). On `ctx.Done()` during `Prompt`: `conn.Cancel(...)` with a 2s budget, then `syscall.Kill(-pid, SIGTERM)`, 10s grace, `SIGKILL`; return `acp.StopReasonCancelled, ctx.Err()`. On a `*acp.RequestError` with `Code == -32000` (auth required): wrap as `fmt.Errorf("agent %s requires authentication; expected %s in the environment: %w", kind, credentialEnv(kind), err)` where `credentialEnv` maps claude-code → `EnvClaudeToken`, codex → `EnvCodexAuth`. After a successful prompt, close stdin, SIGTERM the group, wait up to 5s, SIGKILL; ignore the exit status. `stateDir` passed to `adapterEnv` is `filepath.Join(StateDir, "codex")` for codex and `filepath.Join(StateDir, "home")` for claude-code.

- [ ] **Step 15: Run the agent package tests**

Run: `go test ./internal/agent/... -count=1` → all PASS, including the cancel test on macOS and Linux.

- [ ] **Step 16: Commit**

```bash
git add go.mod go.sum internal/agent
git commit -m "agent: drive Claude Code and Codex over ACP from the runner"
```

---

### Task 4: Workspace goal bootstrap via the agent

**Files:**
- Modify: `internal/workspace/setup.go:273-316` (`runBootstrap`)
- Test: `internal/workspace/setup_test.go`

**Interfaces:**
- Consumes: `agent.FromEnv`, `agent.Run`, `agent.EnvCommand`, `agenttest.Build`.
- Produces: unchanged `SetupWorkspace` signature; `SetupResult.BootstrapRan` is true when the agent turn ended with `end_turn`.

- [ ] **Step 1: Write the failing test**

```go
func TestSetupWorkspace_GoalRunsACPAgent(t *testing.T) {
	stateDir := t.TempDir()
	origAXDir := workspace.AXDir
	workspace.AXDir = stateDir
	defer func() { workspace.AXDir = origAXDir }()
	t.Setenv(agent.EnvKind, "claude-code")
	t.Setenv(agent.EnvCommand, agenttest.Build(t))
	t.Setenv("FAKEAGENT_MODE", "echo")
	wsPath := filepath.Join(t.TempDir(), "ws")

	res, err := workspace.SetupWorkspace(context.Background(), nil, wsPath, "install deps")
	if err != nil {
		t.Fatal(err)
	}
	if !res.BootstrapRan {
		t.Fatal("BootstrapRan = false, want true")
	}
	if _, err := os.Stat(filepath.Join(stateDir, workspace.MarkerName(res.WorkspacePath))); err != nil {
		t.Fatalf("marker not written: %v", err)
	}
}

func TestSetupWorkspace_GoalWithoutAgentFallsBackToAntigravity(t *testing.T) {
	// No AX_AGENT and no GEMINI_API_KEY: the existing path logs and skips.
	t.Setenv(agent.EnvKind, "")
	t.Setenv("GEMINI_API_KEY", "")
	stateDir := t.TempDir()
	origAXDir := workspace.AXDir
	workspace.AXDir = stateDir
	defer func() { workspace.AXDir = origAXDir }()
	res, err := workspace.SetupWorkspace(context.Background(), nil, filepath.Join(t.TempDir(), "ws"), "goal")
	if err != nil || res.BootstrapRan {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}
```

- [ ] **Step 2: Run to verify the first fails**

Run: `go test ./internal/workspace/ -run TestSetupWorkspace_Goal` → first FAIL with `BootstrapRan = false`.

- [ ] **Step 3: Implement**

In `runBootstrap`, before the Antigravity script check:

```go
if kind, model, ok := agent.FromEnv(); ok {
    return runAgentBootstrap(ctx, kind, model, goal, targetPath)
}
```

```go
// runAgentBootstrap hands the goal to the configured ACP agent.
func runAgentBootstrap(ctx context.Context, kind agent.Kind, model, goal, targetPath string) bool
```

It applies `bootstrapTimeout()` via `context.WithTimeout`, calls `agent.Run` with `Env: os.Environ()`, `Output: os.Stdout`, and the prompt: `"Set up the workspace directory " + targetPath + " (your current working directory) according to the following goal: " + goal + ". Only create or modify files inside it."`. Returns `err == nil && reason == acp.StopReasonEndTurn`; logs a warning otherwise (timeout and failure messages mirror the Antigravity ones). Update the `SetupResult.BootstrapRan` comment to say "the goal agent".

- [ ] **Step 4: Run tests**

Run: `go test ./internal/workspace/ -count=1` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/workspace/setup.go internal/workspace/setup_test.go
git commit -m "runner: run the workspace goal on the configured ACP agent"
```

---

### Task 5: Runner — agent task body

**Files:**
- Modify: `runner/runner.go:120-170`
- Test: `runner/runner_test.go`

**Interfaces:**
- Consumes: `agent.FromEnv`, `agent.Run`, `agent.Spec`, `CommandExit`.
- Produces: `OnCommandExit` fires for agent tasks with `ExitCode` 0 on `end_turn`, 1 otherwise, `Pid` 0.

- [ ] **Step 1: Write the failing tests**

```go
func TestRun_AgentTaskRunsPromptInWorkspace(t *testing.T) {
	h := newHarness(t)
	t.Setenv(agent.EnvKind, "claude-code")
	t.Setenv(agent.EnvCommand, agenttest.Build(t))
	t.Setenv("FAKEAGENT_MODE", "echo")
	h.task.Spec.Agent = &v1alpha1.AgentSpec{Prompt: "fix the build"}

	exit := h.runUntilCommandExits(t)
	if exit.Err != nil || exit.ExitCode != 0 {
		t.Fatalf("unexpected exit: %+v", exit)
	}
	// Runner stays up after the turn, like after a command.
	waitHealthy(t, h.cfg.Port)
}

func TestRun_AgentTaskWithoutAgentEnvIdles(t *testing.T) {
	h := newHarness(t)
	t.Setenv(agent.EnvKind, "")
	h.task.Spec.Agent = &v1alpha1.AgentSpec{Prompt: "p"}
	h.start(t)
	waitHealthy(t, h.cfg.Port)
	select {
	case e := <-h.exited:
		t.Fatalf("no agent configured, yet exit reported: %+v", e)
	case <-time.After(300 * time.Millisecond):
	}
	h.cancel()
	h.waitFinished(t, 5*time.Second)
}
```

The exit code and liveness checks are the contract here; Task 3 already proves the prompt round-trip. In `newHarness`, set `agent.StateDir` to `filepath.Join(dir, "ax")` beside the existing `workspace.AXDir` override and restore it in the same cleanup, so no test writes under `/ax`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./runner/ -run TestRun_AgentTask` → first FAIL: no exit reported (agent path missing, runner idles).

- [ ] **Step 3: Implement**

In `Run`, replace the `cmdArgs := ...; if len(cmdArgs) == 0 {...}` block with:

```go
if a := cfg.Task.GetSpec().GetAgent(); a != nil {
    runAgent(ctx, cfg, a, wsPath, metadataURL)
    <-ctx.Done()
    return nil
}
```

```go
// runAgent runs the task's agent prompt and reports it through OnCommandExit.
func runAgent(ctx context.Context, cfg Config, a *v1alpha1.AgentSpec, wsPath, metadataURL string)
```

It calls `agent.FromEnv()`; when `!ok` it logs `slog.Error("task declares spec.agent but no agent is configured; set an ACP Model", ...)` and returns without reporting. Otherwise `agent.Run(ctx, agent.Spec{Kind, Model, Cwd: wsPath, Prompt: a.GetPrompt(), Env: commandEnv(cfg.Task, metadataURL), Output: os.Stdout})`, then `CommandExit{ExitCode: 0 or 1, Err: err}` to `cfg.OnCommandExit` (when set), with a log line naming the stop reason. The `"no task command specified"` branch stays for tasks with neither.

- [ ] **Step 4: Run tests**

Run: `go test ./runner/ -count=1` → PASS.

- [ ] **Step 5: Commit**

```bash
git add runner/runner.go runner/runner_test.go
git commit -m "runner: run spec.agent prompts as the task body"
```

---

### Task 6: Controller — Model resolution and agent env

**Files:**
- Modify: `internal/controller/reconciler.go:56-100` (struct, constructor), `:157-170` (env assembly), add `agentEnv` near `lookupGeminiKey`
- Modify: `cmd/ax-server/main.go:112` (wire `ModelResolver`)
- Test: `internal/controller/reconciler_test.go`

**Interfaces:**
- Produces:
  ```go
  // ModelResolver loads a Model by atespace and name; store.ErrNotFound when absent.
  type ModelResolver func(ctx context.Context, atespace, name string) (*v1alpha1.Model, error)
  // on TaskReconciler, next to SecretResolver:
  ModelResolver ModelResolver
  ```
  Default `nil` means "no Models" (tests that do not set it keep passing). Condition reasons `ModelNotFound`, `UnsupportedProvider`, `CredentialNotFound` set `Ready=False` and `Phase=Failed`.

- [ ] **Step 1: Write the failing tests**

Add a test-local resolver and a helper that reads env out of the last created template:

```go
func modelsOf(ms ...*v1alpha1.Model) controller.ModelResolver {
	return func(_ context.Context, atespace, name string) (*v1alpha1.Model, error) {
		for _, m := range ms {
			if m.GetMetadata().GetAtespace() == atespace && m.GetMetadata().GetName() == name {
				return m, nil
			}
		}
		return nil, store.ErrNotFound
	}
}

func templateEnv(t *testing.T, m *mockControlServer) map[string]string {
	t.Helper()
	if len(m.createdTemplates) == 0 {
		t.Fatal("no ActorTemplate created")
	}
	env := map[string]string{}
	for _, c := range m.createdTemplates[len(m.createdTemplates)-1].GetContainers() {
		for _, e := range c.GetEnv() {
			env[e.GetName()] = e.GetValue()
		}
	}
	return env
}

func claudeModel(name string) *v1alpha1.Model {
	return &v1alpha1.Model{Metadata: &v1alpha1.ObjectMeta{Name: name, Atespace: "default"},
		Spec: &v1alpha1.ModelSpec{Provider: v1alpha1.ProviderClaudeCode, Model: "claude-opus-5-5",
			SecretKey: &v1alpha1.SecretKeyRef{Name: "claude-subscription", Key: "CLAUDE_CODE_OAUTH_TOKEN"}}}
}
```

Three more helpers in the test file:

```go
// newTestReconciler starts the in-process mock Substrate server (as TestTaskReconciler
// does inline), registers cleanup, and returns a reconciler with SecretResolver = noSecrets.
func newTestReconciler(t *testing.T) (*controller.TaskReconciler, *mockControlServer)

// agentTask returns a Task named name in atespace "default" with spec.agent{Model: model, Prompt: "p"}.
func agentTask(name, model string) *v1alpha1.Task

// assertFailed checks err != nil, task.Status.Phase == "Failed", and that the Ready
// condition's Reason equals reason.
func assertFailed(t *testing.T, task *v1alpha1.Task, err error, reason string)
```

Tests:

```go
func TestTaskReconciler_AgentInjectsCredential(t *testing.T) {
	r, mock := newTestReconciler(t)
	r.ModelResolver = modelsOf(claudeModel("default-model"))
	r.SecretResolver = func(_ context.Context, ns, name, key string) (string, error) {
		if ns == "default" && name == "claude-subscription" && key == "CLAUDE_CODE_OAUTH_TOKEN" {
			return "sk-ant-oat-test", nil
		}
		return "", nil
	}
	task := agentTask("t", "") // spec.agent{prompt:"p"}, model ""
	if _, err := r.Reconcile(context.Background(), task, nil); err != nil {
		t.Fatal(err)
	}
	env := templateEnv(t, mock)
	want := map[string]string{"AX_AGENT": "claude-code", "AX_AGENT_MODEL": "claude-opus-5-5", "CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-test"}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
}

func TestTaskReconciler_AgentCodexUsesAuthJSON(t *testing.T) {
	// provider codex, secret key auth.json → CODEX_AUTH_JSON, no CLAUDE_CODE_OAUTH_TOKEN
}

func TestTaskReconciler_AgentModelNotFound(t *testing.T) {
	r, _ := newTestReconciler(t)
	r.ModelResolver = modelsOf()
	task := agentTask("t", "missing")
	_, err := r.Reconcile(context.Background(), task, nil)
	assertFailed(t, task, err, "ModelNotFound")
}

func TestTaskReconciler_AgentUnsupportedProvider(t *testing.T) {
	// default-model provider google → UnsupportedProvider
}

func TestTaskReconciler_AgentCredentialNotFound(t *testing.T) {
	// claude model, SecretResolver returns "" → CredentialNotFound
}

func TestTaskReconciler_CommandTaskIgnoresMissingAgentSecret(t *testing.T) {
	// no spec.agent, claude default-model, noSecrets → Reconcile succeeds,
	// template env has no AX_AGENT and no CLAUDE_CODE_OAUTH_TOKEN
}

func TestTaskReconciler_CommandTaskGetsAgentEnvWhenSecretPresent(t *testing.T) {
	// no spec.agent, claude default-model, secret present → AX_AGENT and token injected
	// (so a Workspace goal on a command task uses the agent)
}
```

- [ ] **Step 2: Run to verify they fail** → compile error on `ModelResolver`.

- [ ] **Step 3: Implement**

```go
// agentEnv resolves the task's Model and returns the agent environment to add
// to the container, or a failure reason and message when the task cannot run.
func (r *TaskReconciler) agentEnv(ctx context.Context, atespace string, task *v1alpha1.Task) (env map[string]string, reason, message string)
```

Logic, from the spec: `name := task.Spec.GetAgent().GetModel()` or `"default-model"`; `wantsAgent := task.Spec.GetAgent() != nil`. `ModelResolver == nil` or `ErrNotFound` → if `wantsAgent` return `ModelNotFound`, else nil. Other resolver error → `ModelNotFound` with the error text when `wantsAgent`, else log and nil. `!v1alpha1.IsACPProvider(provider)` → `UnsupportedProvider` when `wantsAgent`, else nil. Resolve the secret (`SecretResolver` with `secretLookupTimeout`, same as Gemini); empty → `CredentialNotFound` when `wantsAgent`, else `slog.Warn` and nil. Success → `{AX_AGENT: provider, AX_AGENT_MODEL: model (omit when ""), <credential var>: value}` where the credential var is `agent.EnvClaudeToken` for claude-code and `agent.EnvCodexAuth` for codex.

In `Reconcile`, right after the Gemini injection: call `agentEnv`; on a reason, `r.setNotReady(task, reason, message, now); task.Status.Phase = "Failed"; return task, fmt.Errorf("%s: %s", reason, message)`; else merge into `extraEnv`.

In `cmd/ax-server/main.go`, after constructing the reconciler: `reconciler.ModelResolver = rStore.GetModel` (`store.Store.GetModel(ctx, atespace, name)` already has the right signature).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/controller/ ./cmd/... -count=1` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller cmd/ax-server/main.go
git commit -m "controller: ship ACP agent kind and credential to task containers"
```

---

### Task 7: CLI, image, docs, example

**Files:**
- Modify: `cmd/ax/main.go:710-712`
- Modify: `Dockerfile.task-runner`
- Modify: `docs/manifests.md` (Task section near line 32; Model section after line 135), `docs/sandbox.md` (boot steps 3-4, env table), `docs/runner.md` ("What AX sets" table, default runner paragraph), `README.md` ("Why?" table)
- Create: `examples/agent-task.yaml`
- Test: `cmd/ax/main_test.go` (describe output), manual image build

- [ ] **Step 1: `ax describe task` shows the prompt**

After the `Command:` line in `cmd/ax/main.go`:

```go
if a := task.Spec.GetAgent(); a != nil {
    fmt.Printf("Agent:        model=%s\n", orDefault(a.GetModel(), "default-model"))
    fmt.Printf("Prompt:       %q\n", a.GetPrompt())
}
```

`orDefault(s, d string) string` returns `d` when `s` is empty; add it if the file has no equivalent. No describe-output test exists in `cmd/ax` and two `Printf` lines do not earn one; `go vet ./cmd/ax` and the existing suite are the check.

- [ ] **Step 2: Image**

In `Dockerfile.task-runner`, after the apt layer and before the pip layer:

```dockerfile
# Node 22 and the ACP adapters that let a task run Claude Code or Codex.
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g --no-fund --no-audit \
        @agentclientprotocol/claude-agent-acp@<latest at build time> \
        @agentclientprotocol/codex-acp@<latest at build time> \
    && npm cache clean --force
```

Pin both to the exact versions `npm view <pkg> version` prints when you run it. Update the Makefile comment on `build-task-runner` to mention the adapters.

Verify: `docker build --platform linux/amd64 -f Dockerfile.task-runner -t ax-task-runner:acp .` succeeds (needs `bin/linux_amd64/ax-task-runner`; run `make build-task-runner` which does both), then `docker run --rm --platform linux/amd64 --entrypoint sh ax-task-runner:acp -c 'claude-agent-acp --version; codex-acp --version; python3 -c "import google.antigravity"'` prints two versions and no traceback.

- [ ] **Step 3: Example and docs**

`examples/agent-task.yaml`: the Model + Task pair from the spec's "Example manifests" section, plus the matching `kubectl create secret generic claude-subscription --from-literal=CLAUDE_CODE_OAUTH_TOKEN="$(claude setup-token)"` as a comment at the top. Add a second commented block for Codex: `kubectl create secret generic codex-subscription --from-file=auth.json="$HOME/.codex/auth.json"` and `secretKey: {name: codex-subscription, key: auth.json}`.

`docs/manifests.md`: in the Task example add `agent: {prompt: ...}` with a comment that it replaces `command`; new subsection "Running an agent instead of a command". In the Model section replace the `provider: anthropic` block (no code path implements it) with "Claude Code and Codex (subscription)": both secrets, both Models, and the sentence from the spec about `default-model` being the fallback.

`docs/sandbox.md`: step 3 says "hands that goal to the configured ACP agent (Claude Code or Codex) when `AX_AGENT` is set, otherwise to Antigravity"; step 4 covers `spec.agent`; env table gains `AX_AGENT`, `AX_AGENT_MODEL`, `CLAUDE_CODE_OAUTH_TOKEN` / `CODEX_AUTH_JSON` ("set when the task's Model is an ACP provider").

`docs/runner.md`: "What AX sets" table gains the same rows; "Run the command and supervise it" paragraph gains "or, when `spec.agent` is set, run the agent prompt through ACP"; the default runner paragraph lists Node 22 and the two adapters.

`README.md` "Why?" table: new row `Run Claude Code or Codex on your own subscription against a prompt | spec.agent on a Task + a Model`.

- [ ] **Step 4: Full suite and commit**

Run: `gofmt -l . ; go vet ./... && go test ./... -count=1` → no files listed, PASS.

```bash
git add cmd/ax Dockerfile.task-runner Makefile docs README.md examples/agent-task.yaml
git commit -m "docs,cli,image: ACP agents on Claude Code and Codex"
```

---

### Task 8: Real-adapter smoke test (manual, no commit)

**Files:** none changed. Needs the developer's own `claude` login (`~/.claude`) and `codex` login (`~/.codex/auth.json`) on this machine, Node 22 on `PATH`.

- [ ] **Step 1: Build the runner and a scratch task**

```bash
go build -o /tmp/ax-task-runner ./cmd/ax-task-runner
npm install -g @agentclientprotocol/claude-agent-acp@<pinned> @agentclientprotocol/codex-acp@<pinned>
mkdir -p /tmp/ax-smoke/ws && cat > /tmp/ax-smoke/task.yaml <<'EOF'
apiVersion: ax.io/v1alpha1
kind: Task
metadata: {name: smoke}
spec:
  workspace: {name: none, path: /tmp/ax-smoke/ws}
  agent: {prompt: "Create a file named hello.txt containing the single word hello, then stop."}
EOF
```

- [ ] **Step 2: Claude Code**

```bash
AX_AGENT=claude-code CLAUDE_CODE_OAUTH_TOKEN="$(claude setup-token)" \
  /tmp/ax-task-runner --task-file /tmp/ax-smoke/task.yaml --port 18080 --metadata-port 18099
```

Expected: log lines from the agent stream, then `agent turn ended reason=end_turn`, `cat /tmp/ax-smoke/ws/hello.txt` prints `hello`, and `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/readyz` prints `200` while the runner is still up. Ctrl-C exits cleanly.

- [ ] **Step 3: Codex**

```bash
rm -f /tmp/ax-smoke/ws/hello.txt
AX_AGENT=codex CODEX_AUTH_JSON="$(cat ~/.codex/auth.json)" \
  /tmp/ax-task-runner --task-file /tmp/ax-smoke/task.yaml --port 18080 --metadata-port 18099
```

Same expectations. If Codex refuses with `auth_required`, confirm `/ax/codex/auth.json` was written (it needs `/ax` writable locally: `sudo mkdir -p /ax && sudo chown $USER /ax`, or point `agent.StateDir` at a temp dir via a one-off flag if that is easier) and compare the adapter's `INITIAL_AGENT_MODE` / `CODEX_CONFIG` handling against the pinned `codex-acp` source; fall back to `session/set_mode` or a `config.toml` as the spec allows, and fold the fix into Task 3 with a test.

- [ ] **Step 4: Record**

Note the adapter versions and both outcomes in the PR description. Nothing to commit unless Step 3 found a fix.
