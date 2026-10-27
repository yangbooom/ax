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

// Command fakeagent is a minimal ACP agent that tests put in AX_AGENT_COMMAND
// to stand in for claude-agent-acp or codex-acp. FAKEAGENT_MODE selects its
// behavior; see the agent package tests for the modes.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/coder/acp-go-sdk"
)

type fake struct {
	conn *acp.AgentSideConnection
	mode string
}

var _ acp.Agent = (*fake)(nil)

func main() {
	f := &fake{mode: os.Getenv("FAKEAGENT_MODE")}
	if f.mode == "" {
		f.mode = "echo"
	}
	if f.mode == "die" {
		os.Exit(3)
	}
	f.conn = acp.NewAgentSideConnection(f, os.Stdout, os.Stdin)
	<-f.conn.Done()
}

func (f *fake) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}, nil
}

func (f *fake) NewSession(_ context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	if f.mode == "auth-required" {
		return acp.NewSessionResponse{}, acp.NewAuthRequired(nil)
	}
	if err := os.Chdir(params.Cwd); err != nil {
		return acp.NewSessionResponse{}, acp.NewInternalError(err.Error())
	}
	return acp.NewSessionResponse{SessionId: "s1"}, nil
}

func (f *fake) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	cwd, _ := os.Getwd()
	f.say(ctx, params.SessionId, fmt.Sprintf("env: CWD=%s HOME=%s CODEX_HOME=%s NO_BROWSER=%s ANTHROPIC_MODEL=%s CODEX_CONFIG=%s INITIAL_AGENT_MODE=%s",
		cwd, os.Getenv("HOME"), os.Getenv("CODEX_HOME"), os.Getenv("NO_BROWSER"), os.Getenv("ANTHROPIC_MODEL"), os.Getenv("CODEX_CONFIG"), os.Getenv("INITIAL_AGENT_MODE")))

	switch f.mode {
	case "echo":
		text := ""
		if len(params.Prompt) > 0 && params.Prompt[0].Text != nil {
			text = params.Prompt[0].Text.Text
		}
		f.say(ctx, params.SessionId, "echo: "+text)
		_ = f.conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: params.SessionId, Update: acp.StartToolCall("tc-1", "fake tool")})
		_ = f.conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: params.SessionId, Update: acp.UpdateToolCall("tc-1", acp.WithUpdateStatus(acp.ToolCallStatusCompleted))})
	case "permission":
		resp, err := f.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
			SessionId: params.SessionId,
			ToolCall:  acp.ToolCallUpdate{ToolCallId: "tc-perm", Title: acp.Ptr("dangerous thing")},
			Options: []acp.PermissionOption{
				{OptionId: "reject_once", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
				{OptionId: "allow_once", Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
				{OptionId: "allow_always", Name: "Always allow", Kind: acp.PermissionOptionKindAllowAlways},
			},
		})
		switch {
		case err != nil:
			f.say(ctx, params.SessionId, "permission error: "+err.Error())
		case resp.Outcome.Selected != nil:
			f.say(ctx, params.SessionId, "chose: "+string(resp.Outcome.Selected.OptionId))
		default:
			f.say(ctx, params.SessionId, "chose: cancelled")
		}
	case "write":
		_, err := f.conn.WriteTextFile(ctx, acp.WriteTextFileRequest{SessionId: params.SessionId, Path: os.Getenv("FAKEAGENT_WRITE_PATH"), Content: "written"})
		if err != nil {
			f.say(ctx, params.SessionId, "write error: "+err.Error())
		} else {
			f.say(ctx, params.SessionId, "wrote")
		}
	case "hang":
		child := exec.Command("sleep", "300")
		_ = child.Start()
		_ = os.WriteFile(os.Getenv("FAKEAGENT_PID_FILE"), []byte(strconv.Itoa(os.Getpid())), 0o644)
		select {} // never answer; the client must kill us
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (f *fake) say(ctx context.Context, sid acp.SessionId, text string) {
	_ = f.conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: acp.UpdateAgentMessageText(text)})
}

func (f *fake) Cancel(context.Context, acp.CancelNotification) error { return nil }

func (f *fake) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, acp.NewMethodNotFound("authenticate")
}

func (f *fake) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, acp.NewMethodNotFound("logout")
}

func (f *fake) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, nil
}

func (f *fake) ListSessions(context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, acp.NewMethodNotFound("session/list")
}

func (f *fake) ResumeSession(context.Context, acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, acp.NewMethodNotFound("session/resume")
}

func (f *fake) SetSessionConfigOption(context.Context, acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, acp.NewMethodNotFound("session/set_config_option")
}

func (f *fake) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound("session/set_mode")
}
