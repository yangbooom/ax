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
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/coder/acp-go-sdk"
)

// Spec describes one agent run: which agent, where, and what to ask it.
type Spec struct {
	Kind Kind
	// Model is the optional vendor model id; empty lets the agent pick.
	Model string
	// Cwd is the workspace path; it becomes the ACP session's cwd and the
	// boundary for the agent's file access through the runner.
	Cwd    string
	Prompt string
	// Env is the base environment for the adapter process; per-kind variables
	// are appended to it.
	Env []string
	// Output receives a human-readable stream of what the agent is doing.
	// Nil means io.Discard.
	Output io.Writer
}

const (
	// cancelBudget bounds the session/cancel round trip once ctx is done.
	cancelBudget = 2 * time.Second
	// stopGrace is how long the adapter gets after SIGTERM before SIGKILL.
	stopGrace = 5 * time.Second
	// authRequiredCode is the ACP error code for "Authentication required".
	authRequiredCode = -32000
)

// Run starts the adapter for s.Kind, opens one ACP session in s.Cwd, sends
// s.Prompt as a single turn, streams updates to s.Output, and returns the
// stop reason once the turn ends. Cancelling ctx sends session/cancel, then
// SIGTERM, then SIGKILL to the adapter's process group. Run has no timeout of
// its own; callers put a deadline on ctx.
func Run(ctx context.Context, s Spec) (acp.StopReason, error) {
	argv := adapterCommand(s.Kind)
	if len(argv) == 0 {
		return "", fmt.Errorf("no adapter for agent kind %q", s.Kind)
	}
	env, err := adapterEnv(s, filepath.Join(StateDir, stateSubdir(s.Kind)))
	if err != nil {
		return "", err
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = s.Cwd
	cmd.Env = env
	cmd.Stderr = os.Stderr
	// A dedicated process group lets shutdown signal the adapter together with
	// anything it spawned.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("starting adapter %v: %w", argv, err)
	}
	slog.Info("started agent adapter", "agent", s.Kind, "pid", cmd.Process.Pid, "command", argv)

	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	cl := newClient(s.Cwd, s.Output)
	conn := acp.NewClientSideConnection(cl, stdin, stdout)
	// Deferred calls run last-in-first-out: stop the adapter first, so no
	// update can arrive while finish writes the final newline.
	defer cl.finish()
	defer stop(cmd, stdin, exited)

	// Each request races against the adapter dying, so a crash before an
	// answer is an error rather than a hang.
	gone := func(what string) error {
		return fmt.Errorf("adapter %v exited before answering %s", argv, what)
	}
	if _, err := await(ctx, exited, conn.Done(), gone("initialize"), func() (acp.InitializeResponse, error) {
		return conn.Initialize(ctx, acp.InitializeRequest{
			ProtocolVersion: acp.ProtocolVersionNumber,
			ClientCapabilities: acp.ClientCapabilities{
				Fs: acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
			},
		})
	}); err != nil {
		return "", fmt.Errorf("initialize: %w", err)
	}
	sess, err := await(ctx, exited, conn.Done(), gone("session/new"), func() (acp.NewSessionResponse, error) {
		return conn.NewSession(ctx, acp.NewSessionRequest{Cwd: s.Cwd, McpServers: []acp.McpServer{}})
	})
	if err != nil {
		return "", fmt.Errorf("session/new: %w", authHint(s.Kind, err))
	}

	resp, err := await(ctx, exited, conn.Done(), gone("session/prompt"), func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, acp.PromptRequest{SessionId: sess.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock(s.Prompt)}})
	})
	if ctx.Err() != nil {
		cancelCtx, cancel := context.WithTimeout(context.Background(), cancelBudget)
		defer cancel()
		_ = conn.Cancel(cancelCtx, acp.CancelNotification{SessionId: sess.SessionId})
		return acp.StopReasonCancelled, ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("session/prompt: %w", authHint(s.Kind, err))
	}
	slog.Info("agent turn ended", "agent", s.Kind, "reason", resp.StopReason)
	return resp.StopReason, nil
}

// await runs fn and returns its result, unless the adapter exits, the
// connection closes, or ctx ends first.
func await[T any](ctx context.Context, exited, connDone <-chan struct{}, goneErr error, fn func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := fn()
		ch <- result{v, err}
	}()
	var zero T
	select {
	case r := <-ch:
		return r.v, r.err
	case <-exited:
		return zero, goneErr
	case <-connDone:
		return zero, goneErr
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// authHint names the credential the control plane should have shipped when the
// adapter answers "Authentication required".
func authHint(k Kind, err error) error {
	var re *acp.RequestError
	if errors.As(err, &re) && re.Code == authRequiredCode {
		return fmt.Errorf("agent %s requires authentication; expected %s in the environment: %w", k, credentialEnv(k), err)
	}
	return err
}

func credentialEnv(k Kind) string {
	if k == KindCodex {
		return EnvCodexAuth
	}
	return EnvClaudeToken
}

func stateSubdir(k Kind) string {
	if k == KindCodex {
		return codexHomeDir
	}
	return claudeHomeDir
}

// stop shuts the adapter down: close its stdin, SIGTERM its process group,
// and SIGKILL whatever is still there after the grace period.
func stop(cmd *exec.Cmd, stdin io.Closer, exited <-chan struct{}) {
	_ = stdin.Close()
	pid := cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(stopGrace):
		slog.Warn("agent adapter did not exit within grace period; killing", "pid", pid, "gracePeriod", stopGrace)
	}
	// The adapter may have left children in the group; take them all down.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	<-exited
}
