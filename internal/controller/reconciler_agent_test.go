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

package controller_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// newTestReconciler starts an in-process mock Substrate server and returns a
// reconciler pointed at it, with no secrets and a short readiness wait.
func newTestReconciler(t *testing.T) (*controller.TaskReconciler, *mockControlServer) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	mockSrv := &mockControlServer{}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, mockSrv)
	go grpcServer.Serve(lis)
	client, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	t.Cleanup(func() {
		client.Close()
		grpcServer.Stop()
		lis.Close()
	})
	r := controller.NewTaskReconciler(client, "test-template", v1alpha1.DefaultSystemNamespace, "")
	r.SecretResolver = noSecrets
	r.WorkspaceReadyTimeout = 200 * time.Millisecond
	return r, mockSrv
}

// modelsOf is a ModelResolver over a fixed set of Models.
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

// templateEnv returns the container environment of the last ActorTemplate created.
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

func codexModel(name string) *v1alpha1.Model {
	return &v1alpha1.Model{Metadata: &v1alpha1.ObjectMeta{Name: name, Atespace: "default"},
		Spec: &v1alpha1.ModelSpec{Provider: v1alpha1.ProviderCodex,
			SecretKey: &v1alpha1.SecretKeyRef{Name: "codex-subscription", Key: "auth.json"}}}
}

func googleModel(name string) *v1alpha1.Model {
	return &v1alpha1.Model{Metadata: &v1alpha1.ObjectMeta{Name: name, Atespace: "default"},
		Spec: &v1alpha1.ModelSpec{Provider: v1alpha1.ProviderGoogle, Model: "gemini-3.8-flash"}}
}

// secretsOf resolves exactly the given secret name/key pairs.
func secretsOf(values map[string]string) controller.SecretResolver {
	return func(_ context.Context, ns, name, key string) (string, error) {
		if ns != "default" {
			return "", nil
		}
		return values[name+"/"+key], nil
	}
}

// agentTask returns a Task in atespace "default" with spec.agent{Model: model, Prompt: "p"}.
func agentTask(name, model string) *v1alpha1.Task {
	return &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: name, Atespace: "default"},
		Spec:     &v1alpha1.TaskSpec{Agent: &v1alpha1.AgentSpec{Model: model, Prompt: "p"}},
	}
}

func commandTask(name string) *v1alpha1.Task {
	return &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: name, Atespace: "default"},
		Spec:     &v1alpha1.TaskSpec{Command: []string{"/bin/task-runner"}},
	}
}

// assertFailed checks that Reconcile failed the task with the given Ready reason.
func assertFailed(t *testing.T, task *v1alpha1.Task, err error, reason string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want Reconcile error with reason %s, got nil", reason)
	}
	if task.GetStatus().GetPhase() != "Failed" {
		t.Errorf("phase = %q, want Failed", task.GetStatus().GetPhase())
	}
	for _, c := range task.GetStatus().GetConditions() {
		if c.Type == "Ready" {
			if c.Status != "False" || c.Reason != reason {
				t.Errorf("Ready condition = %s/%s, want False/%s", c.Status, c.Reason, reason)
			}
			return
		}
	}
	t.Errorf("no Ready condition on task: %+v", task.GetStatus().GetConditions())
}

func TestTaskReconciler_AgentInjectsCredential(t *testing.T) {
	r, mock := newTestReconciler(t)
	r.ModelResolver = modelsOf(claudeModel("default-model"))
	r.SecretResolver = secretsOf(map[string]string{"claude-subscription/CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-test"})
	task := agentTask("t", "")
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
	if _, ok := env["CODEX_AUTH_JSON"]; ok {
		t.Error("CODEX_AUTH_JSON must not be set for a claude-code model")
	}
}

func TestTaskReconciler_AgentCodexUsesAuthJSON(t *testing.T) {
	r, mock := newTestReconciler(t)
	r.ModelResolver = modelsOf(codexModel("codex-model"))
	r.SecretResolver = secretsOf(map[string]string{"codex-subscription/auth.json": `{"tokens":{}}`})
	task := agentTask("t", "codex-model")
	if _, err := r.Reconcile(context.Background(), task, nil); err != nil {
		t.Fatal(err)
	}
	env := templateEnv(t, mock)
	if env["AX_AGENT"] != "codex" || env["CODEX_AUTH_JSON"] != `{"tokens":{}}` {
		t.Errorf("env = %v", env)
	}
	if _, ok := env["AX_AGENT_MODEL"]; ok {
		t.Error("AX_AGENT_MODEL must be omitted when the Model has no model id")
	}
	if _, ok := env["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Error("CLAUDE_CODE_OAUTH_TOKEN must not be set for a codex model")
	}
}

func TestTaskReconciler_AgentModelNotFound(t *testing.T) {
	r, _ := newTestReconciler(t)
	r.ModelResolver = modelsOf()
	task := agentTask("t", "missing")
	_, err := r.Reconcile(context.Background(), task, nil)
	assertFailed(t, task, err, "ModelNotFound")
}

func TestTaskReconciler_AgentUnsupportedProvider(t *testing.T) {
	r, _ := newTestReconciler(t)
	r.ModelResolver = modelsOf(googleModel("default-model"))
	task := agentTask("t", "")
	_, err := r.Reconcile(context.Background(), task, nil)
	assertFailed(t, task, err, "UnsupportedProvider")
}

func TestTaskReconciler_AgentCredentialNotFound(t *testing.T) {
	r, _ := newTestReconciler(t)
	r.ModelResolver = modelsOf(claudeModel("default-model"))
	task := agentTask("t", "")
	_, err := r.Reconcile(context.Background(), task, nil)
	assertFailed(t, task, err, "CredentialNotFound")
}

func TestTaskReconciler_CommandTaskIgnoresMissingAgentSecret(t *testing.T) {
	r, mock := newTestReconciler(t)
	r.ModelResolver = modelsOf(claudeModel("default-model"))
	task := commandTask("t")
	if _, err := r.Reconcile(context.Background(), task, nil); err != nil {
		t.Fatalf("command task must not fail on a missing agent secret: %v", err)
	}
	env := templateEnv(t, mock)
	for _, k := range []string{"AX_AGENT", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s must not be set without a credential", k)
		}
	}
}

func TestTaskReconciler_CommandTaskGetsAgentEnvWhenSecretPresent(t *testing.T) {
	r, mock := newTestReconciler(t)
	r.ModelResolver = modelsOf(claudeModel("default-model"))
	r.SecretResolver = secretsOf(map[string]string{"claude-subscription/CLAUDE_CODE_OAUTH_TOKEN": "tok"})
	task := commandTask("t")
	if _, err := r.Reconcile(context.Background(), task, nil); err != nil {
		t.Fatal(err)
	}
	env := templateEnv(t, mock)
	if env["AX_AGENT"] != "claude-code" || env["CLAUDE_CODE_OAUTH_TOKEN"] != "tok" {
		t.Errorf("agent env missing on command task: %v", env)
	}
}
