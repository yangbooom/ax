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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
)

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

func TestClient_SessionUpdate_JoinsStreamedChunks(t *testing.T) {
	var buf bytes.Buffer
	c := newClient(t.TempDir(), &buf)
	ctx := context.Background()
	// Agents stream a message a few tokens at a time; the log must read as prose.
	for _, chunk := range []string{"Created", " `hello", ".txt`", " with hello."} {
		_ = c.SessionUpdate(ctx, acp.SessionNotification{Update: acp.UpdateAgentMessageText(chunk)})
	}
	_ = c.SessionUpdate(ctx, acp.SessionNotification{Update: acp.StartToolCall("tc", "run tests", acp.WithStartStatus(acp.ToolCallStatusPending))})
	_ = c.SessionUpdate(ctx, acp.SessionNotification{Update: acp.StartToolCall("tc2", "no status")})
	_ = c.SessionUpdate(ctx, acp.SessionNotification{Update: acp.UpdateAgentMessageText("Done.")})
	c.finish()
	want := "Created `hello.txt` with hello.\n[tool] run tests (pending)\n[tool] no status\nDone.\n"
	if got := buf.String(); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}
