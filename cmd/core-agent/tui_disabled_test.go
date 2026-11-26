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

//go:build no_tui

package main

import "testing"

func TestMakeMCPElicitor_DisabledInSlimBuild(t *testing.T) {
	t.Parallel()
	for _, interactive := range []bool{false, true} {
		fn, handle := makeMCPElicitor(interactive)
		if fn != nil || handle != nil {
			t.Errorf("makeMCPElicitor(%v) = (%v, %v), want (nil, nil) in -tags no_tui build", interactive, fn != nil, handle)
		}
	}
}
