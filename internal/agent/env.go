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

// Package agent drives a coding agent (Claude Code or Codex) over the Agent
// Client Protocol from inside a task container. The control plane decides
// which agent and ships its credential as environment variables; this package
// reads them, starts the vendor's ACP adapter as a child process, and runs a
// single prompt through it.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

// Kind names an agent AX can drive. The values are the Model provider strings.
type Kind string

const (
	KindClaudeCode Kind = v1alpha1.ProviderClaudeCode
	KindCodex      Kind = v1alpha1.ProviderCodex
)

// Environment variables that carry the agent configuration into the container.
// The control plane sets them from the task's Model; the runner reads them.
const (
	// EnvKind selects the agent: "claude-code" or "codex".
	EnvKind = "AX_AGENT"
	// EnvModel is the optional vendor model id passed to the agent.
	EnvModel = "AX_AGENT_MODEL"
	// EnvCommand overrides the adapter command line (space-separated argv).
	// It exists for local runs and tests; the image puts the adapters on PATH.
	EnvCommand = "AX_AGENT_COMMAND"
	// EnvClaudeToken is the Claude subscription token from `claude setup-token`.
	// The Claude Code CLI inside the adapter reads it natively.
	EnvClaudeToken = "CLAUDE_CODE_OAUTH_TOKEN"
	// EnvCodexAuth holds the contents of a `codex login` auth.json. The runner
	// writes it to CODEX_HOME before starting the adapter.
	EnvCodexAuth = "CODEX_AUTH_JSON"
)

// StateDir holds adapter state outside the workspace: <StateDir>/codex is
// CODEX_HOME and <StateDir>/home is the HOME fallback for Claude Code. It
// defaults to the same directory as workspace.AXDir; tests override it.
var StateDir = "/ax"

const (
	codexHomeDir   = "codex"
	claudeHomeDir  = "home"
	codexAuthFile  = "auth.json"
	stateDirPerm   = 0o700
	credentialPerm = 0o600
)

// adapterCommands maps each agent to the adapter binary the image installs.
var adapterCommands = map[Kind][]string{
	KindClaudeCode: {"claude-agent-acp"},
	KindCodex:      {"codex-acp"},
}

// FromEnv reports the agent configured for this container, if any. ok is
// false when AX_AGENT is unset or names an agent this package does not know.
func FromEnv() (kind Kind, model string, ok bool) {
	kind = Kind(os.Getenv(EnvKind))
	if _, known := adapterCommands[kind]; !known {
		return "", "", false
	}
	return kind, os.Getenv(EnvModel), true
}

// adapterCommand returns the adapter argv for k, honoring EnvCommand when set.
func adapterCommand(k Kind) []string {
	if override := strings.Fields(os.Getenv(EnvCommand)); len(override) > 0 {
		return override
	}
	return adapterCommands[k]
}

// adapterEnv returns s.Env plus the variables the adapter for s.Kind needs.
// stateDir is where adapter state lives for this run: CODEX_HOME for Codex,
// the HOME fallback for Claude Code. Later entries win, so a per-kind value
// overrides anything in s.Env.
func adapterEnv(s Spec, stateDir string) ([]string, error) {
	env := append([]string(nil), s.Env...)
	switch s.Kind {
	case KindCodex:
		if err := os.MkdirAll(stateDir, stateDirPerm); err != nil {
			return nil, fmt.Errorf("creating CODEX_HOME %s: %w", stateDir, err)
		}
		if auth := lookup(env, EnvCodexAuth); auth != "" {
			// Create-only: Codex rewrites auth.json when it refreshes its token,
			// and a later run in the same container (goal setup, then the task
			// body) must not put the stale copy from the Secret back.
			if err := writeIfMissing(filepath.Join(stateDir, codexAuthFile), auth); err != nil {
				return nil, fmt.Errorf("writing codex auth.json: %w", err)
			}
		}
		env = append(env,
			"CODEX_HOME="+stateDir,
			// Never advertise browser login: nobody is at a keyboard in the sandbox.
			"NO_BROWSER=1",
			// The Substrate sandbox is the isolation boundary; Codex's own would
			// only get in the way.
			"INITIAL_AGENT_MODE=agent-full-access",
		)
		if s.Model != "" {
			cfg, _ := json.Marshal(map[string]string{"model": s.Model})
			env = append(env, "CODEX_CONFIG="+string(cfg))
		}
	case KindClaudeCode:
		if s.Model != "" {
			env = append(env, "ANTHROPIC_MODEL="+s.Model)
		}
		// The Claude Code CLI writes ~/.claude; give it a writable HOME.
		if !writableDir(lookup(env, "HOME")) {
			if err := os.MkdirAll(stateDir, stateDirPerm); err != nil {
				return nil, fmt.Errorf("creating HOME fallback %s: %w", stateDir, err)
			}
			env = append(env, "HOME="+stateDir)
		}
	default:
		return nil, fmt.Errorf("unknown agent kind %q", s.Kind)
	}
	return env, nil
}

// writeIfMissing creates path with content and credential permissions, and
// leaves an existing file alone.
func writeIfMissing(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, credentialPerm)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// lookup returns the value of key in env; the last entry wins.
func lookup(env []string, key string) string {
	val := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			val = strings.TrimPrefix(kv, key+"=")
		}
	}
	return val
}

// writableDir reports whether dir exists and a file can be created in it.
func writableDir(dir string) bool {
	if dir == "" {
		return false
	}
	f, err := os.CreateTemp(dir, ".ax-probe-*")
	if err != nil {
		return false
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	return true
}
