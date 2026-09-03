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

package attach

import "testing"

// #896. The status-update frame sent on stream open is the only
// turn-state information a client attaching to an already-running
// session ever gets: typed frames are live fan-out with no replay, and
// a per-incident watcher session is born together with its first turn,
// so there is no window in which to attach beforehand.
//
// The pre-fix mapping keyed on State alone. Since pause outranks
// running in a one-field State, the parked-mid-turn case seeded
// turn_state:"idle" over a turn that was still executing — which is
// exactly what the #799 F4 operator saw.
func TestStatusSnapshot_SeedsStreamingWheneverATurnIsInFlight(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   StatusInfo
		want string
	}{
		{
			name: "running",
			in:   StatusInfo{State: AgentStateRunning, TurnInFlight: true},
			want: TurnStateStreaming,
		},
		{
			// The window #896 was filed on.
			name: "paused with the interrupted turn still executing",
			in:   StatusInfo{State: AgentStatePaused, TurnInFlight: true},
			want: TurnStateStreaming,
		},
		{
			name: "quiet hold",
			in:   StatusInfo{State: AgentStatePaused},
			want: TurnStateIdle,
		},
		{
			name: "idle",
			in:   StatusInfo{State: AgentStateIdle},
			want: TurnStateIdle,
		},
		{
			// A pre-1.12.0 registrant that produces State but not the
			// bool must keep working off State alone.
			name: "running with no bool set",
			in:   StatusInfo{State: AgentStateRunning},
			want: TurnStateStreaming,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.in.ModelName = "m"
			b := &broadcaster{entry: &Entry{Agent: &richRegistrant{
				stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
				status:         tc.in,
			}}}
			got := b.statusSnapshot()
			if got.TurnState != tc.want {
				t.Errorf("turn_state = %q, want %q for %+v", got.TurnState, tc.want, tc.in)
			}
			if got.Model != "m" {
				t.Errorf("model = %q, want %q", got.Model, "m")
			}
		})
	}
}
