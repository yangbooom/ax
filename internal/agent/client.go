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
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/coder/acp-go-sdk"
)

// client is the ACP client side the adapter talks to. Nobody is at a
// keyboard, so permission requests are approved, file access is confined to
// the workspace, and session updates are streamed to out as plain lines.
type client struct {
	cwd string
	out io.Writer
	// mu serializes writes to out: the SDK delivers notifications on its own
	// goroutine while Run may call finish from another.
	mu sync.Mutex
	// midLine is true while the last message chunk written did not end a line,
	// so the next non-message line can start on a line of its own.
	midLine bool
}

var _ acp.Client = (*client)(nil)

func newClient(cwd string, out io.Writer) *client {
	if out == nil {
		out = io.Discard
	}
	return &client{cwd: cwd, out: out}
}

// RequestPermission picks the broadest allow option offered: allow_always,
// then allow_once, then whatever comes first. The sandbox is the boundary.
func (c *client) RequestPermission(_ context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	if len(params.Options) == 0 {
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}}, nil
	}
	chosen := params.Options[0]
	for _, kind := range []acp.PermissionOptionKind{acp.PermissionOptionKindAllowAlways, acp.PermissionOptionKindAllowOnce} {
		if o, ok := findOption(params.Options, kind); ok {
			chosen = o
			break
		}
	}
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Selected: &acp.RequestPermissionOutcomeSelected{OptionId: chosen.OptionId}}}, nil
}

func findOption(opts []acp.PermissionOption, kind acp.PermissionOptionKind) (acp.PermissionOption, bool) {
	for _, o := range opts {
		if o.Kind == kind {
			return o, true
		}
	}
	return acp.PermissionOption{}, false
}

// SessionUpdate writes what the agent is doing to out. Message chunks are
// streamed as the agent sends them, a few tokens at a time, so they are
// written without added line breaks; tool calls and plans get a line each.
// Thought chunks are dropped: they are not part of the agent's answer.
func (c *client) SessionUpdate(_ context.Context, params acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := params.Update
	switch {
	case u.AgentMessageChunk != nil:
		if t := u.AgentMessageChunk.Content.Text; t != nil && t.Text != "" {
			fmt.Fprint(c.out, t.Text)
			c.midLine = !strings.HasSuffix(t.Text, "\n")
		}
	case u.ToolCall != nil:
		c.endLine()
		if u.ToolCall.Status == "" {
			fmt.Fprintf(c.out, "[tool] %s\n", u.ToolCall.Title)
		} else {
			fmt.Fprintf(c.out, "[tool] %s (%s)\n", u.ToolCall.Title, u.ToolCall.Status)
		}
	case u.ToolCallUpdate != nil:
		if u.ToolCallUpdate.Status != nil {
			c.endLine()
			fmt.Fprintf(c.out, "[tool] %s %s\n", u.ToolCallUpdate.ToolCallId, *u.ToolCallUpdate.Status)
		}
	case u.Plan != nil:
		c.endLine()
		for _, e := range u.Plan.Entries {
			fmt.Fprintf(c.out, "[plan] %s %s\n", e.Status, e.Content)
		}
	}
	return nil
}

// finish ends an unterminated message line, if there is one. Call it once
// the adapter can no longer send updates.
func (c *client) finish() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.endLine()
}

// endLine is finish for callers that already hold mu.
func (c *client) endLine() {
	if c.midLine {
		fmt.Fprintln(c.out)
		c.midLine = false
	}
}

func (c *client) ReadTextFile(_ context.Context, params acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	path, err := c.confine(params.Path)
	if err != nil {
		return acp.ReadTextFileResponse{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return acp.ReadTextFileResponse{}, err
	}
	content := string(b)
	if params.Line != nil || params.Limit != nil {
		lines := strings.Split(content, "\n")
		start := 0
		if params.Line != nil && *params.Line > 0 {
			start = min(*params.Line-1, len(lines))
		}
		end := len(lines)
		if params.Limit != nil && *params.Limit > 0 && start+*params.Limit < end {
			end = start + *params.Limit
		}
		content = strings.Join(lines[start:end], "\n")
	}
	return acp.ReadTextFileResponse{Content: content}, nil
}

func (c *client) WriteTextFile(_ context.Context, params acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	path, err := c.confine(params.Path)
	if err != nil {
		return acp.WriteTextFileResponse{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return acp.WriteTextFileResponse{}, err
	}
	return acp.WriteTextFileResponse{}, os.WriteFile(path, []byte(params.Content), 0o644)
}

// confine returns p cleaned, or an error when p is relative or outside cwd.
func (c *client) confine(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path must be absolute: %s", p)
	}
	p = filepath.Clean(p)
	rel, err := filepath.Rel(c.cwd, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %s is outside the workspace %s", p, c.cwd)
	}
	return p, nil
}

// Terminal methods are never called: the terminal capability is not
// advertised, so the adapters run shell commands themselves.
var errNoTerminal = errors.New("terminal not supported")

func (c *client) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errNoTerminal
}

func (c *client) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errNoTerminal
}

func (c *client) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errNoTerminal
}

func (c *client) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errNoTerminal
}

func (c *client) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errNoTerminal
}
