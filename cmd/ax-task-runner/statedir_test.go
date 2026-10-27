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

package main

import (
	"testing"

	"github.com/google/ax/internal/agent"
	"github.com/google/ax/internal/workspace"
)

func TestApplyStateDir_SetsWorkspaceAndAgentDirs(t *testing.T) {
	origAX, origAgent := workspace.AXDir, agent.StateDir
	t.Cleanup(func() { workspace.AXDir, agent.StateDir = origAX, origAgent })

	applyStateDir("")
	if workspace.AXDir != origAX || agent.StateDir != origAgent {
		t.Fatalf("empty flag must leave defaults: %q %q", workspace.AXDir, agent.StateDir)
	}
	applyStateDir("/tmp/ax-state")
	if workspace.AXDir != "/tmp/ax-state" || agent.StateDir != "/tmp/ax-state" {
		t.Fatalf("got %q %q, want both /tmp/ax-state", workspace.AXDir, agent.StateDir)
	}
}
