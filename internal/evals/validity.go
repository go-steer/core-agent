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
	ViolationUnknownTool     = pkgevals.ViolationUnknownTool
	ViolationMissingRequired = pkgevals.ViolationMissingRequired
	ViolationWrongType       = pkgevals.ViolationWrongType
	ViolationNotInEnum       = pkgevals.ViolationNotInEnum
	ViolationUndeclaredArg   = pkgevals.ViolationUndeclaredArg
	ViolationErrorResult     = pkgevals.ViolationErrorResult
	ViolationEmptyResult     = pkgevals.ViolationEmptyResult
	ViolationNoCompletion    = pkgevals.ViolationNoCompletion
	digestResult             = 200
)

type (
	Violation   = pkgevals.Violation
	CallRecord  = pkgevals.CallRecord
	ToolSchemas = pkgevals.ToolSchemas
)

func RecordCalls(calls []Call) []CallRecord {
	return pkgevals.RecordCalls(calls)
}

func ValidateCalls(schemas ToolSchemas, calls []Call, emptyResult func(Call) bool) []Violation {
	return pkgevals.ValidateCalls(schemas, calls, emptyResult)
}

func ViolationCounts(vs []Violation) []string {
	return pkgevals.ViolationCounts(vs)
}
