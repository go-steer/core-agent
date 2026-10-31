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

package gemini

import (
	"google.golang.org/adk/v2/session"

	"github.com/go-steer/core-models/adkv2"
	cmgemini "github.com/go-steer/core-models/dialect/gemini"
)

// GroundingProjection wraps a session.Service so that every event
// carrying Gemini Google Search grounding is followed by synthetic
// events surfacing that evidence under AuthorGoogleSearch: one per web
// search query the model issued, then one per grounded web source
// (title + URI). They share the parent event's InvocationID and Branch
// and have an empty Content.Role, so ADK leaves them out of the
// conversation it sends the model.
//
// Deprecated: the projection lives in core-models now; call
// adkv2.GroundingProjection (github.com/go-steer/core-models/adkv2)
// directly. This wrapper delegates to it and stays for embedders who
// wired the core-agent name. Removed in v4.0.
func GroundingProjection(inner session.Service) session.Service {
	return adkv2.GroundingProjection(inner)
}

// AuthorGoogleSearch is the synthetic event Author the grounding
// projection uses for both "model issued a search query" and "model
// grounded on this web source" events. Stable across releases so
// eventlog consumers can filter on it.
const AuthorGoogleSearch = cmgemini.AuthorGoogleSearch
