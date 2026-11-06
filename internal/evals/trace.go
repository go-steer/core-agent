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

import (
	adksession "google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v3/internal/effects"
	pkgevals "github.com/go-steer/core-agent/v3/pkg/evals"
)

// Trace, Call, and Class re-export pkg/evals (#1538).
type (
	Trace = pkgevals.Trace
	Call  = pkgevals.Call
	Class = pkgevals.Class
)

const (
	ClassReadOnly = pkgevals.ClassReadOnly
	ClassMutating = pkgevals.ClassMutating
	ClassSpawning = pkgevals.ClassSpawning
)

// TraceFromEvents extracts a Trace from a recorded session event log.
func TraceFromEvents(events adksession.Events, pred effects.Predicate, subAgents map[string]bool) Trace {
	var pkgPred pkgevals.Predicate
	if pred != nil {
		pkgPred = func(name string) pkgevals.Class {
			return pkgevals.Class(pred(name))
		}
	}
	return pkgevals.TraceFromEvents(events, pkgPred, subAgents)
}

func isControl(name string, subAgents map[string]bool) bool {
	return pkgevals.IsControlCall(name, subAgents)
}
