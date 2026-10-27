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

package model

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestAgentCredentialFrom_RequiresLabel(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("sk-ant-oat-test\n"))
	unlabeled := &secretObject{Data: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": encoded}}
	if _, err := agentCredentialFrom(unlabeled, "CLAUDE_CODE_OAUTH_TOKEN"); err == nil || !strings.Contains(err.Error(), AgentCredentialLabel) {
		t.Fatalf("unlabeled secret must be refused with a hint about %s, got %v", AgentCredentialLabel, err)
	}

	labeled := &secretObject{Data: unlabeled.Data}
	labeled.Metadata.Labels = map[string]string{AgentCredentialLabel: "true"}
	got, err := agentCredentialFrom(labeled, "CLAUDE_CODE_OAUTH_TOKEN")
	if err != nil || got != "sk-ant-oat-test" {
		t.Fatalf("labeled secret: got %q, %v", got, err)
	}

	if _, err := agentCredentialFrom(labeled, "missing"); err == nil {
		t.Fatal("missing key must be an error")
	}

	wrongValue := &secretObject{Data: unlabeled.Data}
	wrongValue.Metadata.Labels = map[string]string{AgentCredentialLabel: "yes"}
	if _, err := agentCredentialFrom(wrongValue, "CLAUDE_CODE_OAUTH_TOKEN"); err == nil {
		t.Fatal("label value other than \"true\" must be refused")
	}
}
