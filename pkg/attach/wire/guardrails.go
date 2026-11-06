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

package wire

// Guardrail names accepted by GuardrailResetRequest.Guardrail and
// reported in GuardrailResetResponse.Reset (#666). Shared with the
// durable trip row (#643) and the `guardrail-trip` frame, so an
// operator UI has one set of names to know.
const (
	// GuardrailWatchdog is the behavioral watchdog (enforce mode).
	GuardrailWatchdog = "watchdog"
	// GuardrailCostCeiling is the per-turn / per-session spend cap.
	GuardrailCostCeiling = "cost_ceiling"
	// GuardrailAll targets every guardrail that is currently tripped.
	// The default when the request names none.
	GuardrailAll = "all"
)
