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
	"fmt"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// egressPolicyName is the name of an actor's single EgressPolicy.
const egressPolicyName = "default"

// EnsureActorEgressPolicy makes the actor's EgressPolicy allow TLS to hostnames
// on port 443, passed through without interception, and nothing else. An
// existing policy with other rules is replaced; one that already matches is
// left alone. No hostnames means nothing is touched, so Substrate's
// default-deny stays in force.
func (c *Client) EnsureActorEgressPolicy(ctx context.Context, atespace, actorName string, hostnames []string) error {
	if len(hostnames) == 0 {
		return nil
	}
	actor := &ateapipb.ObjectRef{Atespace: atespace, Name: actorName}
	rules := []*ateapipb.EgressRule{{TlsPassthrough: &ateapipb.TlsPassthroughRule{
		Hostnames: hostnames,
		Ports:     &ateapipb.Ports{Numbers: []int32{443}},
	}}}

	var existing *ateapipb.EgressPolicy
	err := retryOnCodes(ctx, func() error {
		var err error
		existing, err = c.control.GetActorEgressPolicy(ctx, &ateapipb.GetActorEgressPolicyRequest{Actor: actor, Name: egressPolicyName})
		return err
	}, codes.Unavailable)
	switch {
	case status.Code(err) == codes.NotFound:
		err = retryOnCodes(ctx, func() error {
			_, err := c.control.CreateActorEgressPolicy(ctx, &ateapipb.CreateActorEgressPolicyRequest{
				Actor: actor,
				// The server requires the singleton's metadata on create.
				EgressPolicy: &ateapipb.EgressPolicy{
					Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: egressPolicyName},
					Rules:    rules,
				},
			})
			return err
		}, codes.Unavailable)
		if err != nil {
			return fmt.Errorf("creating egress policy for actor %s/%s: %w", atespace, actorName, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("getting egress policy for actor %s/%s: %w", atespace, actorName, err)
	}

	if proto.Equal(&ateapipb.EgressPolicy{Rules: existing.GetRules()}, &ateapipb.EgressPolicy{Rules: rules}) {
		return nil
	}
	// The existing metadata carries the version the server checks on update.
	err = retryOnCodes(ctx, func() error {
		_, err := c.control.UpdateActorEgressPolicy(ctx, &ateapipb.UpdateActorEgressPolicyRequest{
			Actor:        actor,
			EgressPolicy: &ateapipb.EgressPolicy{Metadata: existing.GetMetadata(), Rules: rules},
		})
		return err
	}, codes.Unavailable)
	if err != nil {
		return fmt.Errorf("updating egress policy for actor %s/%s: %w", atespace, actorName, err)
	}
	return nil
}
