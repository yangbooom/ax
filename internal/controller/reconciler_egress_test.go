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
	"slices"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func actorKey(ref *ateapipb.ObjectRef) string {
	return ref.GetAtespace() + "/" + ref.GetName()
}

func (m *mockControlServer) GetActorEgressPolicy(_ context.Context, req *ateapipb.GetActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	if p, ok := m.egressPolicies[actorKey(req.GetActor())]; ok {
		return p, nil
	}
	return nil, status.Error(codes.NotFound, "no egress policy")
}

func (m *mockControlServer) CreateActorEgressPolicy(_ context.Context, req *ateapipb.CreateActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	if m.egressPolicyErr != nil {
		return nil, m.egressPolicyErr
	}
	// Substrate requires the singleton's metadata on create (kind, v0.5.0)
	// and refuses a policy for an actor that does not exist yet.
	if md := req.GetEgressPolicy().GetMetadata(); md.GetName() != "default" || md.GetAtespace() != req.GetActor().GetAtespace() {
		return nil, status.Error(codes.InvalidArgument, "egress_policy.metadata: Required value")
	}
	if !slices.Contains(m.createdActors, req.GetActor().GetName()) {
		return nil, status.Error(codes.FailedPrecondition, "parent Actor does not exist")
	}
	if m.egressPolicies == nil {
		m.egressPolicies = map[string]*ateapipb.EgressPolicy{}
	}
	// Records whether the controller got the order wrong: the policy must be
	// in place before the actor is ever suspended, so it is there at resume.
	m.egressPolicyAfterSuspend = slices.Contains(m.suspendedActors, req.GetActor().GetName())
	p := proto.Clone(req.GetEgressPolicy()).(*ateapipb.EgressPolicy)
	p.Metadata = &ateapipb.ResourceMetadata{Atespace: req.GetActor().GetAtespace(), Name: "default", Version: 1}
	m.egressPolicies[actorKey(req.GetActor())] = p
	return p, nil
}

func (m *mockControlServer) UpdateActorEgressPolicy(_ context.Context, req *ateapipb.UpdateActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	if m.egressPolicyErr != nil {
		return nil, m.egressPolicyErr
	}
	p := proto.Clone(req.GetEgressPolicy()).(*ateapipb.EgressPolicy)
	if p.GetMetadata() == nil {
		return nil, status.Error(codes.InvalidArgument, "EgressPolicy UID and version are required")
	}
	// Like the server, every update bumps the version.
	p.Metadata.Version = m.egressPolicies[actorKey(req.GetActor())].GetMetadata().GetVersion() + 1
	m.egressPolicies[actorKey(req.GetActor())] = p
	return p, nil
}

func egressTask(name string, hosts ...string) *v1alpha1.Task {
	task := commandTask(name)
	task.Spec.Egress = &v1alpha1.EgressSpec{Hosts: hosts}
	return task
}

func wantPassthrough(hosts ...string) *ateapipb.EgressPolicy {
	return &ateapipb.EgressPolicy{Rules: []*ateapipb.EgressRule{{TlsPassthrough: &ateapipb.TlsPassthroughRule{
		Hostnames: hosts,
		Ports:     &ateapipb.Ports{Numbers: []int32{443}},
	}}}}
}

func TestReconcile_EgressPolicyExistsBeforeFirstSuspend(t *testing.T) {
	r, m := newTestReconciler(t)
	task, err := r.Reconcile(context.Background(), egressTask("hello", "api.anthropic.com", "*.anthropic.com"), nil)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if task.GetStatus().GetPhase() != "Suspended" {
		t.Fatalf("phase = %q, want Suspended", task.GetStatus().GetPhase())
	}
	p := m.egressPolicies["default/hello"]
	if p == nil {
		t.Fatal("no egress policy created for actor default/hello")
	}
	if want := wantPassthrough("api.anthropic.com", "*.anthropic.com"); !proto.Equal(&ateapipb.EgressPolicy{Rules: p.GetRules()}, want) {
		t.Errorf("policy rules = %v, want %v", p.GetRules(), want.GetRules())
	}
	if m.egressPolicyAfterSuspend {
		t.Error("policy was created after the actor was suspended; it must exist before any resume")
	}

	// The resume pass finds the policy in place and leaves it alone.
	m.workerIP = "127.0.0.1:1"
	task.Status.Phase = "Running"
	if _, err := r.Reconcile(context.Background(), task, nil); err != nil {
		t.Fatalf("Reconcile (resume): %v", err)
	}
	if got := m.egressPolicies["default/hello"]; got.GetMetadata().GetVersion() != 1 {
		t.Errorf("unchanged policy was rewritten: %v", got)
	}
}

func TestReconcile_NoEgressCreatesNoPolicy(t *testing.T) {
	r, m := newTestReconciler(t)
	if _, err := r.Reconcile(context.Background(), commandTask("plain"), nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(m.egressPolicies) != 0 {
		t.Errorf("task without spec.egress got a policy: %v", m.egressPolicies)
	}
}

func TestReconcile_EgressPolicyRejectedFailsTask(t *testing.T) {
	r, m := newTestReconciler(t)
	m.egressPolicyErr = status.Error(codes.InvalidArgument, "hostname rejected")
	task, err := r.Reconcile(context.Background(), egressTask("bad", "api.anthropic.com"), nil)
	assertFailed(t, task, err, "EgressPolicyFailed")
}

func TestReconcile_EgressPolicyUnavailableLeavesTaskPending(t *testing.T) {
	r, m := newTestReconciler(t)
	m.egressPolicyErr = status.Error(codes.Unavailable, "substrate down")
	task, err := r.Reconcile(context.Background(), egressTask("later", "api.anthropic.com"), nil)
	if err == nil {
		t.Fatal("want an error while Substrate is unavailable")
	}
	if task.GetStatus().GetPhase() != "Pending" {
		t.Errorf("phase = %q, want Pending", task.GetStatus().GetPhase())
	}
	for _, c := range task.GetStatus().GetConditions() {
		if c.Type == "Ready" && c.Reason != "SubstrateUnavailable" {
			t.Errorf("Ready reason = %q, want SubstrateUnavailable", c.Reason)
		}
	}
}
