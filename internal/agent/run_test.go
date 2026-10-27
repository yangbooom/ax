// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/google/ax/internal/agent/agenttest"
)

// lookupEnv returns the value of key in env; the last entry wins, as it does
// for a process environment.
func lookupEnv(env []string, key string) string {
	val := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			val = strings.TrimPrefix(kv, key+"=")
		}
	}
	return val
}

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
	// The credential arrives in Spec.Env, the adapter's base environment, not
	// necessarily in the runner's own process environment.
	env, err := adapterEnv(Spec{Kind: KindCodex, Model: "m", Env: []string{EnvCodexAuth + `={"tokens":{}}`}}, home)
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

func TestAdapterEnv_Codex_KeepsRefreshedAuth(t *testing.T) {
	// Codex rewrites auth.json when it refreshes its token. A second run in the
	// same container (goal setup, then the task body) must not put the stale
	// copy from the Secret back, or the refresh token gets reused.
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"refreshed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := adapterEnv(Spec{Kind: KindCodex, Env: []string{EnvCodexAuth + `={"stale":true}`}}, home); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(home, "auth.json")); got != `{"refreshed":true}` {
		t.Fatalf("auth.json was overwritten: %s", got)
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

// fakeSpec points Run at the fake agent in the given mode. The returned
// Spec's Output is a *bytes.Buffer.
func fakeSpec(t *testing.T, mode string, extra ...string) Spec {
	t.Helper()
	t.Setenv(EnvCommand, agenttest.Build(t))
	t.Setenv("FAKEAGENT_MODE", mode)
	var buf bytes.Buffer
	return Spec{Kind: KindClaudeCode, Cwd: t.TempDir(), Prompt: "ping", Output: &buf,
		Env: append(os.Environ(), extra...)}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func waitForFile(t *testing.T, p string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(p); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", p)
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRun_EchoesPromptAndEndsTurn(t *testing.T) {
	s := fakeSpec(t, "echo")
	reason, err := Run(context.Background(), s)
	if err != nil || reason != acp.StopReasonEndTurn {
		t.Fatalf("reason=%q err=%v", reason, err)
	}
	out := s.Output.(*bytes.Buffer).String()
	for _, want := range []string{"echo: ping", "fake tool", "CWD=" + mustEval(t, s.Cwd)} {
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
	start := time.Now()
	_, err := Run(ctx, s)
	// A dead adapter must be reported as such, promptly: a context timeout
	// here would mean Run hung until the deadline.
	if err == nil || !strings.Contains(err.Error(), "exited before answering initialize") {
		t.Fatalf("want 'exited before answering initialize', got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Run took %v to notice the dead adapter", elapsed)
	}
}

func TestRun_CancelDuringSessionNewReturns(t *testing.T) {
	s := fakeSpec(t, "hang-session")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := Run(ctx, s); done <- err }()
	time.Sleep(300 * time.Millisecond) // let initialize complete and session/new hang
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error after cancel during session/new")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancel during session/new")
	}
}

func TestRun_CancelKillsAdapterProcessGroup(t *testing.T) {
	s := fakeSpec(t, "hang")
	pidFile := filepath.Join(t.TempDir(), "pid")
	s.Env = append(s.Env, "FAKEAGENT_PID_FILE="+pidFile)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := Run(ctx, s); done <- err }()
	waitForFile(t, pidFile)
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
