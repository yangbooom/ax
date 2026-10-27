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

package substrate

import (
	"context"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// egressMock is the slice of the Control API that EnsureActorEgressPolicy uses.
type egressMock struct {
	ateapipb.ControlClient
	existing *ateapipb.EgressPolicy // nil answers NotFound
	gets     int
	created  *ateapipb.CreateActorEgressPolicyRequest
	updated  *ateapipb.UpdateActorEgressPolicyRequest
}

func (m *egressMock) GetActorEgressPolicy(_ context.Context, _ *ateapipb.GetActorEgressPolicyRequest, _ ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	m.gets++
	if m.existing == nil {
		return nil, status.Error(codes.NotFound, "no egress policy")
	}
	return m.existing, nil
}

func (m *egressMock) CreateActorEgressPolicy(_ context.Context, req *ateapipb.CreateActorEgressPolicyRequest, _ ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	m.created = req
	return req.GetEgressPolicy(), nil
}

func (m *egressMock) UpdateActorEgressPolicy(_ context.Context, req *ateapipb.UpdateActorEgressPolicyRequest, _ ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	m.updated = req
	return req.GetEgressPolicy(), nil
}

func passthroughRules(hosts ...string) []*ateapipb.EgressRule {
	return []*ateapipb.EgressRule{{TlsPassthrough: &ateapipb.TlsPassthroughRule{
		Hostnames: hosts,
		Ports:     &ateapipb.Ports{Numbers: []int32{443}},
	}}}
}

func TestEnsureActorEgressPolicy_CreatesTLSPassthroughRule(t *testing.T) {
	m := &egressMock{}
	c := &Client{control: m}
	if err := c.EnsureActorEgressPolicy(context.Background(), "default", "hello", []string{"api.anthropic.com", "*.anthropic.com"}); err != nil {
		t.Fatalf("EnsureActorEgressPolicy: %v", err)
	}
	if m.created == nil || m.updated != nil {
		t.Fatalf("want one Create and no Update, got create=%v update=%v", m.created, m.updated)
	}
	if ref := m.created.GetActor(); ref.GetAtespace() != "default" || ref.GetName() != "hello" {
		t.Errorf("create targets %s/%s, want default/hello", ref.GetAtespace(), ref.GetName())
	}
	// The real server rejects a Create whose metadata is missing; the singleton
	// is named "default" in the actor's atespace.
	want := &ateapipb.EgressPolicy{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "default", Name: "default"},
		Rules:    passthroughRules("api.anthropic.com", "*.anthropic.com"),
	}
	if !proto.Equal(m.created.GetEgressPolicy(), want) {
		t.Errorf("created policy = %v, want %v", m.created.GetEgressPolicy(), want)
	}
}

func TestEnsureActorEgressPolicy_UpdatesWhenRulesDiffer(t *testing.T) {
	m := &egressMock{existing: &ateapipb.EgressPolicy{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "default", Name: "default", Version: 3},
		Rules:    passthroughRules("old.example.com"),
	}}
	c := &Client{control: m}
	if err := c.EnsureActorEgressPolicy(context.Background(), "default", "hello", []string{"api.anthropic.com"}); err != nil {
		t.Fatalf("EnsureActorEgressPolicy: %v", err)
	}
	if m.created != nil || m.updated == nil {
		t.Fatalf("want one Update and no Create, got create=%v update=%v", m.created, m.updated)
	}
	got := m.updated.GetEgressPolicy()
	if !proto.Equal(got.GetMetadata(), m.existing.GetMetadata()) {
		t.Errorf("update must carry the existing metadata (version), got %v", got.GetMetadata())
	}
	if !proto.Equal(&ateapipb.EgressPolicy{Rules: got.GetRules()}, &ateapipb.EgressPolicy{Rules: passthroughRules("api.anthropic.com")}) {
		t.Errorf("updated rules = %v", got.GetRules())
	}
}

func TestEnsureActorEgressPolicy_NoopWhenAlreadyEqual(t *testing.T) {
	m := &egressMock{existing: &ateapipb.EgressPolicy{
		Metadata: &ateapipb.ResourceMetadata{Name: "default", Version: 1},
		Rules:    passthroughRules("api.anthropic.com"),
	}}
	c := &Client{control: m}
	if err := c.EnsureActorEgressPolicy(context.Background(), "default", "hello", []string{"api.anthropic.com"}); err != nil {
		t.Fatalf("EnsureActorEgressPolicy: %v", err)
	}
	if m.created != nil || m.updated != nil {
		t.Fatalf("policy already matches; want no writes, got create=%v update=%v", m.created, m.updated)
	}
}

func TestEnsureActorEgressPolicy_NoHostnamesTouchesNothing(t *testing.T) {
	m := &egressMock{}
	c := &Client{control: m}
	if err := c.EnsureActorEgressPolicy(context.Background(), "default", "hello", nil); err != nil {
		t.Fatalf("EnsureActorEgressPolicy: %v", err)
	}
	if m.gets != 0 || m.created != nil || m.updated != nil {
		t.Fatal("no hostnames must mean no Substrate calls")
	}
}
