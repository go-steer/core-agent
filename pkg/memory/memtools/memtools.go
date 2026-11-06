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

// Package memtools builds the model-facing long-term memory tools
// (recall_memory, persist_memory, forget_memory) over a *memory.Router
// (#948, docs/memory-service-design.md "Model-facing tools").
//
// Each handler calls the permission gate itself with a mount-qualified
// key ("mount=<mount> ..."), so operator policies written in the gate's
// tool:glob grammar can tell private writes from shared ones, for
// example "persist_memory:mount=agent *".
//
// The package lives outside pkg/memory so that package stays a leaf
// that does not import ADK's tool layer or pkg/permissions.
package memtools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/memory"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

// Tool names. Classified in pkg/permissions (builtinToolClasses),
// internal/effects (builtinClasses) and pkg/tools (readOnlyBuiltins).
const (
	RecallToolName  = "recall_memory"
	PersistToolName = "persist_memory"
	ForgetToolName  = "forget_memory"
)

// maxKeyDetail caps the free-text part of a gate key so a long fact or
// query cannot make the prompt line unreadable.
const maxKeyDetail = 120

// Options configures New.
type Options struct {
	// Gate is consulted inside every handler. Nil leaves the tools
	// ungated, which is what a host that wired no gate has everywhere.
	Gate *permissions.Gate
	// DefaultTopK is used when recall_memory is called without top_k.
	// Zero falls back to the Router's own default.
	DefaultTopK int
}

// New returns the memory tools for r. recall_memory is returned when r
// has at least one mount; persist_memory and forget_memory are returned
// only when at least one mount is writable, so a read-only deployment
// never advertises a write that would always fail. A nil Router yields
// no tools.
func New(r *memory.Router, opts Options) ([]tool.Tool, error) {
	if r == nil || len(r.Mounts()) == 0 {
		return nil, nil
	}
	h := &handlers{router: r, gate: opts.Gate, topK: opts.DefaultTopK}
	recall, err := functiontool.New(functiontool.Config{
		Name:        RecallToolName,
		Description: recallDescription(r),
	}, h.recall)
	if err != nil {
		return nil, fmt.Errorf("memtools: %s: %w", RecallToolName, err)
	}
	out := []tool.Tool{recall}
	if !r.HasWritableMount() {
		return out, nil
	}
	persist, err := functiontool.New(functiontool.Config{
		Name:        PersistToolName,
		Description: persistDescription(r),
	}, h.persist)
	if err != nil {
		return nil, fmt.Errorf("memtools: %s: %w", PersistToolName, err)
	}
	forget, err := functiontool.New(functiontool.Config{
		Name:        ForgetToolName,
		Description: forgetDescription(r),
	}, h.forget)
	if err != nil {
		return nil, fmt.Errorf("memtools: %s: %w", ForgetToolName, err)
	}
	return append(out, persist, forget), nil
}

type handlers struct {
	router *memory.Router
	gate   *permissions.Gate
	topK   int
}

// RecallArgs are recall_memory's arguments.
type RecallArgs struct {
	Query string `json:"query" jsonschema:"the facts, constraints, earlier findings or preferences being looked for, in plain words"`
	Mount string `json:"mount,omitempty" jsonschema:"which mounted scope to search: all (the default), agent, or one shared:<pool> name"`
	TopK  int    `json:"top_k,omitempty" jsonschema:"maximum number of memories to return, 1 to 50"`
}

// RecalledItem is one memory as recall_memory returns it.
type RecalledItem struct {
	ID        string    `json:"id"`
	Mount     string    `json:"mount"`
	ReadOnly  bool      `json:"read_only"`
	Fact      string    `json:"fact"`
	Topics    []string  `json:"topics,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// RecallResult is recall_memory's result.
type RecallResult struct {
	Items    []RecalledItem        `json:"items"`
	Warnings []memory.MountWarning `json:"warnings,omitempty"`
}

// PermittedRecallMounts returns the mounts a recall across "all" may
// search: every mount except those an operator deny rule names for
// recall_memory (for example "recall_memory:mount=shared:secret *").
// Only deny rules are consulted, so this never prompts and stays
// usable for pre-turn preload in plan mode. A nil gate permits every
// mount.
func PermittedRecallMounts(ctx context.Context, gate *permissions.Gate, r *memory.Router, query string) []string {
	ms := r.Mounts()
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if gate != nil {
			key := "mount=" + m.Name + " query=" + truncate(query)
			if err := gate.CheckPolicyDeny(ctx, RecallToolName, key); err != nil && strings.Contains(err.Error(), "denied by config policy") {
				continue
			}
		}
		out = append(out, m.Name)
	}
	return out
}

func (h *handlers) recall(ctx adkagent.Context, in RecallArgs) (RecallResult, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return RecallResult{}, fmt.Errorf("%s: query is required", RecallToolName)
	}
	mount := strings.TrimSpace(in.Mount)
	if mount == "" {
		mount = memory.MountAll
	}
	if h.gate != nil {
		key := "mount=" + mount + " query=" + truncate(query)
		if err := h.gate.CheckReadOnlyToolCallWithArgs(ctx, RecallToolName, RecallToolName, key, in); err != nil {
			return RecallResult{}, err
		}
	}
	topK := in.TopK
	if topK <= 0 {
		topK = h.topK
	}
	targets := []string{mount}
	if mount == memory.MountAll {
		// "all" must not reach a scope a per-mount deny rule names.
		targets = PermittedRecallMounts(ctx, h.gate, h.router, query)
		if len(targets) == 0 {
			return RecallResult{Items: []RecalledItem{}}, nil
		}
	}
	items, warnings, err := h.router.Recall(withUser(ctx), query, targets, nil, topK)
	if err != nil {
		return RecallResult{}, err
	}
	out := RecallResult{Items: make([]RecalledItem, 0, len(items)), Warnings: warnings}
	for _, it := range items {
		out.Items = append(out.Items, RecalledItem{
			ID:        it.ID,
			Mount:     it.Mount,
			ReadOnly:  it.ReadOnly,
			Fact:      it.Fact,
			Topics:    it.Topics,
			UpdatedAt: it.UpdatedAt,
		})
	}
	return out, nil
}

// PersistArgs are persist_memory's arguments.
type PersistArgs struct {
	Fact   string   `json:"fact" jsonschema:"one self-contained statement to keep across sessions: a verified fact, constraint, preference or rule, in plain text, never executable code"`
	Mount  string   `json:"mount,omitempty" jsonschema:"writable scope to store it in; defaults to agent when agent is writable, otherwise one writable shared:<pool> name must be given"`
	Topics []string `json:"topics,omitempty" jsonschema:"short labels that categorize the fact"`
}

// PersistResult is persist_memory's result.
type PersistResult struct {
	Mount  string                      `json:"mount"`
	Deltas []memory.ConsolidationDelta `json:"deltas"`
	// Warning is set when the write succeeded but recording it in the
	// audit trail failed. The write must not be retried.
	Warning string `json:"warning,omitempty"`
}

func (h *handlers) persist(ctx adkagent.Context, in PersistArgs) (PersistResult, error) {
	fact := strings.TrimSpace(in.Fact)
	if fact == "" {
		return PersistResult{}, fmt.Errorf("%s: fact is required", PersistToolName)
	}
	mount := strings.TrimSpace(in.Mount)
	if mount == "" && h.agentWritable() {
		mount = memory.MountAgent
	}
	if mount == "" {
		return PersistResult{}, fmt.Errorf("%s: mount is required because the agent scope is not writable; use one of %s",
			PersistToolName, strings.Join(writableMounts(h.router), ", "))
	}
	if h.gate != nil {
		key := "mount=" + mount + " fact=" + truncate(fact)
		if err := h.gate.CheckGenericWithArgs(ctx, PersistToolName, key, in); err != nil {
			return PersistResult{}, err
		}
	}
	deltas, err := h.router.Remember(withUser(ctx), mount, []memory.CandidateFact{{Fact: fact, Topics: in.Topics}})
	if errors.Is(err, memory.ErrAuditFailed) {
		return PersistResult{Mount: mount, Deltas: deltas, Warning: auditWarning}, nil
	}
	if err != nil {
		return PersistResult{}, err
	}
	return PersistResult{Mount: mount, Deltas: deltas}, nil
}

// ForgetArgs are forget_memory's arguments.
type ForgetArgs struct {
	ID    string `json:"id" jsonschema:"id of the memory to delete, as returned by recall_memory"`
	Mount string `json:"mount" jsonschema:"writable scope that owns the memory: agent or one shared:<pool> name"`
}

// ForgetResult is forget_memory's result.
type ForgetResult struct {
	Mount   string `json:"mount"`
	ID      string `json:"id"`
	Deleted bool   `json:"deleted"`
	// Warning: see PersistResult.Warning.
	Warning string `json:"warning,omitempty"`
}

// auditWarning tells the model a write landed even though its audit
// record did not, so it does not repeat the write.
const auditWarning = "the write succeeded; recording it in the audit trail failed, so do not repeat it"

func (h *handlers) forget(ctx adkagent.Context, in ForgetArgs) (ForgetResult, error) {
	id := strings.TrimSpace(in.ID)
	mount := strings.TrimSpace(in.Mount)
	if id == "" || mount == "" {
		return ForgetResult{}, fmt.Errorf("%s: id and mount are both required", ForgetToolName)
	}
	if h.gate != nil {
		key := "mount=" + mount + " id=" + truncate(id)
		if err := h.gate.CheckGenericWithArgs(ctx, ForgetToolName, key, in); err != nil {
			return ForgetResult{}, err
		}
	}
	err := h.router.Forget(withUser(ctx), mount, id)
	if errors.Is(err, memory.ErrAuditFailed) {
		return ForgetResult{Mount: mount, ID: id, Deleted: true, Warning: auditWarning}, nil
	}
	if err != nil {
		return ForgetResult{}, err
	}
	return ForgetResult{Mount: mount, ID: id, Deleted: true}, nil
}

func (h *handlers) agentWritable() bool {
	for _, m := range h.router.Mounts() {
		if m.Name == memory.MountAgent {
			return m.Mode == memory.AccessReadWrite
		}
	}
	return false
}

// withUser makes sure a user_id is on ctx for mounts that partition by
// user: an id already stamped by the agent wins, then the verified
// attach caller, then the runner's session user.
func withUser(ctx adkagent.Context) context.Context {
	if memory.UserIDFromContext(ctx) != "" {
		return ctx
	}
	uid := ""
	if c, ok := auth.CallerFromContext(ctx); ok {
		uid = c.Identity
	}
	if uid == "" {
		uid = ctx.UserID()
	}
	if uid == "" {
		return ctx
	}
	return memory.WithUserID(ctx, uid)
}

func truncate(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxKeyDetail {
		return s
	}
	cut := maxKeyDetail
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

func mountNames(r *memory.Router) []string {
	ms := r.Mounts()
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		name := m.Name
		if m.Mode == memory.AccessReadOnly {
			name += " (read-only)"
		}
		out = append(out, name)
	}
	return out
}

func writableMounts(r *memory.Router) []string {
	var out []string
	for _, m := range r.Mounts() {
		if m.Mode == memory.AccessReadWrite {
			out = append(out, m.Name)
		}
	}
	return out
}

func recallDescription(r *memory.Router) string {
	return "Search long-term memory kept across sessions and return matching memories, each tagged with the scope it came from and whether that scope is read-only. " +
		"Mounted scopes: " + strings.Join(mountNames(r), ", ") + ". " +
		"The agent scope holds this agent's own memories; shared:<pool> scopes are shared with other agents. " +
		"Recalled memories can be out of date; check them against current state before acting on one."
}

func persistDescription(r *memory.Router) string {
	return "Store one fact in long-term memory so later sessions can recall it. " +
		"Writable scopes: " + strings.Join(writableMounts(r), ", ") + ". " +
		"A fact that restates or updates an existing memory in the same scope may be merged into it rather than stored twice. " +
		"Store only what is verified and worth keeping beyond this session, never credentials or other secrets."
}

func forgetDescription(r *memory.Router) string {
	return "Delete one memory from a writable long-term memory scope, by the id recall_memory returned. " +
		"Writable scopes: " + strings.Join(writableMounts(r), ", ") + ". " +
		"The id must belong to the named scope; a memory in another scope is not deleted."
}
