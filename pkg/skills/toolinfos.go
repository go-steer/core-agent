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

package skills

import (
	"context"
	"iter"
	"sort"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// ToolInfos returns the TOOLS this bundle exposes to the model, which
// is a different list from Infos.
//
// Infos is the skills themselves — one entry per SKILL.md discovered.
// The model never calls those by name; it calls the small fixed set of
// tools the skill toolset registers (list_skills / load_skill /
// load_skill_resource) and names a skill as an argument. Operator
// surfaces that answer "what tools does this agent have?" need this
// list; surfaces that answer "what skills are installed?" need Infos.
//
// Nil when no skills were discovered (Empty), because the toolset —
// and therefore the tools — only exists once there is something to
// load. Sorted by name.
//
// Enumeration is in-memory: the ADK skill toolset builds its tools at
// construction and the gate wrapper only re-wraps them, so this is
// safe to call from a request path.
func (s Skills) ToolInfos() []Info {
	if s.Toolset == nil {
		return nil
	}
	tools, err := s.Toolset.Tools(listCtx{Context: context.Background()})
	if err != nil {
		return nil
	}
	out := make([]Info, 0, len(tools))
	for _, t := range tools {
		out = append(out, Info{Name: t.Name(), Description: t.Description()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// listCtx is a read-only stub satisfying adkagent.ReadonlyContext, used
// only to enumerate the toolset's tools. The skill toolset returns a
// slice it built at construction and ignores the context entirely; the
// gate wrapper forwards without reading it either. Mirrors the
// equivalent stub in pkg/mcp, which exists for the same reason.
type listCtx struct{ context.Context }

func (listCtx) UserContent() *genai.Content          { return nil }
func (listCtx) InvocationID() string                 { return "" }
func (listCtx) AgentName() string                    { return "" }
func (listCtx) UserID() string                       { return "" }
func (listCtx) AppName() string                      { return "" }
func (listCtx) SessionID() string                    { return "" }
func (listCtx) Branch() string                       { return "" }
func (listCtx) ReadonlyState() session.ReadonlyState { return emptyState{} }

type emptyState struct{}

func (emptyState) Get(string) (any, error) { return nil, session.ErrStateKeyNotExist }
func (emptyState) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {}
}

// compile-time check that the stub still satisfies the ADK interface.
var _ adkagent.ReadonlyContext = listCtx{}
