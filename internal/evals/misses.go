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

package evals

import pkgevals "github.com/go-steer/core-agent/v3/pkg/evals"

type (
	ConsequentialMiss = pkgevals.ConsequentialMiss
	MissReport        = pkgevals.MissReport
	ToolGating        = pkgevals.ToolGating
)

func ClassifyMisses(tbl IntentTable, sc Scenario, tr Trace) MissReport {
	return pkgevals.ClassifyMisses(tbl, sc, tr)
}

func GatingBy(tbl IntentTable, ds Dataset) map[string]ToolGating {
	return pkgevals.GatingBy(tbl, ds)
}
