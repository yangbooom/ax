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

// Package agenttest builds the fakeagent binary for tests that need an ACP
// adapter without network access or credentials.
package agenttest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var (
	once sync.Once
	path string
	err  error
)

// Build compiles the fakeagent binary once per test process and returns its
// path. Tests put it in AX_AGENT_COMMAND to stand in for a real adapter.
func Build(t testing.TB) string {
	t.Helper()
	once.Do(func() {
		path = filepath.Join(os.TempDir(), fmt.Sprintf("ax-fakeagent-%d", os.Getpid()))
		out, buildErr := exec.Command("go", "build", "-o", path, "github.com/google/ax/internal/agent/fakeagent").CombinedOutput()
		if buildErr != nil {
			err = fmt.Errorf("building fakeagent: %v\n%s", buildErr, out)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return path
}
