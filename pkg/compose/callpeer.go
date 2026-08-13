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

package compose

import (
	"time"

	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/tools/peer"
)

// PeerDirectory adapts a hub's peer registry to the roster the
// call_peer tool reads (#595). getReg is a closure, not a registry,
// because the tool is built with the rest of the tool catalog while
// the registry is constructed later in boot, inside the attach block.
// It may return nil — a nil registry reads as an empty roster, and
// call_peer says "no peers are registered" rather than panicking.
//
// now is injectable for tests; nil means time.Now.
//
// Expired leases are filtered here rather than trusted to the
// registry's 5-second prune tick. A peer that stopped heartbeating is
// presumed gone, and dialing it burns the caller's whole timeout on a
// pod that isn't there.
func PeerDirectory(getReg func() *attach.PeerRegistry, now func() time.Time) peer.Directory {
	if now == nil {
		now = time.Now
	}
	return peer.DirectoryFunc(func() []peer.Peer {
		if getReg == nil {
			return nil
		}
		reg := getReg()
		if reg == nil {
			return nil
		}
		live := reg.List(nil)
		out := make([]peer.Peer, 0, len(live))
		t := now()
		for _, p := range live {
			if p == nil || p.LeaseExpiresAt.Before(t) {
				continue
			}
			out = append(out, peer.Peer{
				Name:     p.Name,
				Endpoint: p.Endpoint,
				Labels:   p.Labels,
			})
		}
		return out
	})
}

// BuildCallPeerTool constructs the call_peer built-in over a hub's
// peer registry. Callers gate the call on cfg.Tools.CallPeer.Enabled
// AND the daemon actually being a hub; see cmd/core-agent, which
// refuses to start when the first is set without the second.
func BuildCallPeerTool(gate *permissions.Gate, cfg *config.Config, getReg func() *attach.PeerRegistry) (adktool.Tool, error) {
	return peer.New(gate, cfg, PeerDirectory(getReg, nil))
}
