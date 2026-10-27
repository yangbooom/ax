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

package v1alpha1_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

func TestValidateTask_EgressHostnames(t *testing.T) {
	valid := []string{"api.anthropic.com", "*.anthropic.com", "claude.ai", "*", "a-b.c0"}
	for _, h := range valid {
		task := &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "t"}, Spec: &v1alpha1.TaskSpec{
			Egress: &v1alpha1.EgressSpec{Hosts: []string{h}},
		}}
		if err := v1alpha1.ValidateTask(task); err != nil {
			t.Errorf("hostname %q: valid name rejected: %v", h, err)
		}
	}
	// Substrate also refuses a last label made only of digits, which
	// net.ParseIP does not catch for "a.123" or "999.1.1.1".
	invalid := []string{"", "API.anthropic.com", "https://api.anthropic.com", "api.anthropic.com:443", "10.0.0.1", "*.*.com", "a.*.com", "foo..com", "-a.com", "a.com.", "*.", "123", "a.123", "999.1.1.1"}
	for _, h := range invalid {
		task := &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "t"}, Spec: &v1alpha1.TaskSpec{
			Egress: &v1alpha1.EgressSpec{Hosts: []string{h}},
		}}
		err := v1alpha1.ValidateTask(task)
		if err == nil || !strings.Contains(err.Error(), "spec.egress") {
			t.Errorf("hostname %q: want a spec.egress error, got %v", h, err)
		}
	}
}

// Substrate treats the host list as a set of at most 256 entries; catching
// that at apply time saves the user a Failed task they cannot edit.
func TestValidateTask_EgressListLimits(t *testing.T) {
	dup := &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "t"}, Spec: &v1alpha1.TaskSpec{
		Egress: &v1alpha1.EgressSpec{Hosts: []string{"api.anthropic.com", "claude.ai", "api.anthropic.com"}},
	}}
	if err := v1alpha1.ValidateTask(dup); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate host: want a duplicate error, got %v", err)
	}
	many := make([]string, 257)
	for i := range many {
		many[i] = fmt.Sprintf("h%d.example.com", i)
	}
	tooMany := &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "t"}, Spec: &v1alpha1.TaskSpec{Egress: &v1alpha1.EgressSpec{Hosts: many}}}
	if err := v1alpha1.ValidateTask(tooMany); err == nil || !strings.Contains(err.Error(), "256") {
		t.Errorf("257 hosts: want a limit error mentioning 256, got %v", err)
	}
	tooMany.Spec.Egress.Hosts = many[:256]
	if err := v1alpha1.ValidateTask(tooMany); err != nil {
		t.Errorf("256 hosts must be accepted: %v", err)
	}
}
