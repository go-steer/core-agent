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

const (
	MetricIntentCoverage   = pkgevals.MetricIntentCoverage
	MetricToolCoverage     = pkgevals.MetricToolCoverage
	MetricSeverityAccuracy = pkgevals.MetricSeverityAccuracy
	MetricEffectOrdering   = pkgevals.MetricEffectOrdering
	MetricExactlyOnce      = pkgevals.MetricExactlyOnce
)

// MetricResult re-exports pkg/evals.MetricResult (#1538).
type MetricResult = pkgevals.MetricResult

func EvaluateAll(tbl IntentTable, sc Scenario, tr Trace) []MetricResult {
	return pkgevals.EvaluateAll(tbl, sc, tr)
}

func IntentCoverage(tbl IntentTable, sc Scenario, tr Trace) MetricResult {
	return pkgevals.IntentCoverage(tbl, sc, tr)
}

func ToolCoverage(tbl IntentTable, sc Scenario, tr Trace) MetricResult {
	return pkgevals.ToolCoverage(tbl, sc, tr)
}

func SeverityAccuracy(sc Scenario, tr Trace) MetricResult {
	return pkgevals.SeverityAccuracy(sc, tr)
}

func EffectOrdering(tr Trace) MetricResult {
	return pkgevals.EffectOrdering(tr)
}

func ExactlyOnce(tr Trace) MetricResult {
	return pkgevals.ExactlyOnce(tr)
}

func extractSeverity(s string) string {
	return pkgevals.ExtractSeverity(s)
}
