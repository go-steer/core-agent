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
	"sync/atomic"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/attachadapter"
)

// TestCoreAgentAdapter_ModeChipFollowsModelSwap pins the target of the
// local mode chip across a /model swap (#1168). The chip's Set is wired
// once at launch, but SwitchModel builds a new agent on the same
// session. If the chip kept writing its audit row through the
// launch-time agent, that agent (never running a turn again) would
// append in the middle of the new agent's turn and fail it as stale.
func TestCoreAgentAdapter_ModeChipFollowsModelSwap(t *testing.T) {
	t.Parallel()
	ad, inner := wakeAdapter(t)
	ad.attachAd = attachadapter.New(inner)
	ad.permAd = &atomic.Pointer[attachadapter.Adapter]{}
	ad.permAd.Store(ad.attachAd)

	next, err := ad.SwitchModel("gemini-3.5-pro")
	if err != nil {
		t.Fatalf("SwitchModel: %v", err)
	}
	child := next.(*coreAgentAdapter)

	if got := ad.permModeAdapter(); got != child.attachAd {
		t.Error("after /model the chip wired at launch still writes through the retired agent")
	}
	if got := child.permModeAdapter(); got != child.attachAd {
		t.Error("the /model successor's chip does not write through its own agent")
	}
}

// TestCoreAgentAdapter_ModeChipWithoutSharedTarget covers the bare
// adapters other tests build: no shared target, so the adapter's own
// attachAd is used rather than a nil.
func TestCoreAgentAdapter_ModeChipWithoutSharedTarget(t *testing.T) {
	t.Parallel()
	ad, inner := wakeAdapter(t)
	ad.attachAd = attachadapter.New(inner)
	if ad.permModeAdapter() != ad.attachAd {
		t.Error("a bare adapter's chip should fall back to its own attachAd")
	}
}
