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

package workspace_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/ax/internal/agent"
	"github.com/google/ax/internal/agent/agenttest"
	"github.com/google/ax/internal/workspace"
)

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
