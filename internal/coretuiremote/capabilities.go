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

package coretuiremote

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/pkg/attach"
)

// ===== Status / Tools / Subagents (read-only capabilities) =====

// Status satisfies coretui.StatusReporter. State is "idle" by
// default; the attach status endpoint doesn't yet distinguish
// "running" / "deferred" (see pkg/agent's AttachStatus). Errors
// fall back to a zero Status — the TUI shows "—" instead of stale
// data.
func (a *Adapter) Status() coretui.Status {
	info, err := a.client.Status(context.TODO(), a.sessionPath)
	if err != nil {
		return coretui.Status{}
	}
	return coretui.Status{
		ModelName: info.ModelName,
		State:     info.State,
	}
}

// Tools satisfies coretui.ToolLister. Backs /tools.
func (a *Adapter) Tools() []coretui.ToolInfo {
	infos, err := a.client.Tools(context.TODO(), a.sessionPath)
	if err != nil {
		return nil
	}
	out := make([]coretui.ToolInfo, 0, len(infos))
	for _, t := range infos {
		// Source maps "builtin" verbatim; MCP-sourced tools surface
		// the server name (the existing pkg/attach ToolInfo carries
		// it in Server but coretui.ToolInfo wants it flattened into
		// Source).
		source := t.Source
		if t.Server != "" {
			source = t.Server
		}
		out = append(out, coretui.ToolInfo{
			Name:        t.Name,
			Description: t.Description,
			Source:      source,
			GateState:   t.GateState,
		})
	}
	return out
}

// Subagents satisfies coretui.SubagentLister. Backs /subagents.
func (a *Adapter) Subagents() []coretui.SubagentInfo {
	infos, err := a.client.Agents(context.TODO(), a.sessionPath)
	if err != nil {
		return nil
	}
	out := make([]coretui.SubagentInfo, 0, len(infos))
	for _, ai := range infos {
		out = append(out, coretui.SubagentInfo{
			Name:       ai.Name,
			Status:     ai.Status,
			LastReport: ai.LastReport,
			StartedAt:  ai.StartedAt,
		})
	}
	return out
}

// ===== Usage tracker (cached) =====
//
// coretui.UsageTracker is a 7-method read-only interface the TUI
// snapshots on every render. Hitting the network on every call
// would be wasteful; cache with a short TTL so /stats and the
// per-turn footer stay close to fresh without amplifying traffic.

const usageCacheTTL = 2 * time.Second

type usageCache struct {
	mu        sync.Mutex
	cached    coretui.Usage
	costUSD   float64
	turns     int
	lastFetch time.Time
}

// snapshot returns the cached usage, refreshing if the cache is
// older than usageCacheTTL. The network call happens inline; if it
// fails, the prior cache stays in effect (the TUI displays the
// last-known-good value rather than flickering to zero).
func (a *Adapter) snapshot() (coretui.Usage, float64, int) {
	a.usage.mu.Lock()
	defer a.usage.mu.Unlock()
	if !a.usage.lastFetch.IsZero() && time.Since(a.usage.lastFetch) < usageCacheTTL {
		return a.usage.cached, a.usage.costUSD, a.usage.turns
	}
	info, err := a.client.Usage(context.TODO(), a.sessionPath)
	if err != nil {
		// Bump lastFetch to throttle retries on persistent failure.
		a.usage.lastFetch = time.Now()
		return a.usage.cached, a.usage.costUSD, a.usage.turns
	}
	a.usage.cached = coretui.Usage{
		InputTokens:  int(info.Overall.InputTokens),
		OutputTokens: int(info.Overall.OutputTokens),
	}
	a.usage.costUSD = info.Overall.CostUSD
	a.usage.turns = info.Overall.Turns
	a.usage.lastFetch = time.Now()
	return a.usage.cached, a.usage.costUSD, a.usage.turns
}

// SessionTotals satisfies coretui.UsageTracker.
func (a *Adapter) SessionTotals() coretui.Usage {
	u, _, _ := a.snapshot()
	return u
}

// SessionCostUSD satisfies coretui.UsageTracker.
func (a *Adapter) SessionCostUSD() float64 {
	_, cost, _ := a.snapshot()
	return cost
}

// LastTurn satisfies coretui.UsageTracker. Returns the most recent
// per-turn token counts captured during Run() and the matching cost
// (computed client-side from cached per-Mtok rates). Falls back to
// $0 cost when the pricing snapshot hasn't been fetched yet or the
// model has no published rate.
func (a *Adapter) LastTurn() (coretui.Usage, float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cost := costFromRates(a.lastTurn.InputTokens, a.lastTurn.OutputTokens, a.pricingIn, a.pricingOut)
	return a.lastTurn, cost
}

// applyPricing populates ev.CostUSD + ev.Model when we have rates
// cached, so the coretui per-turn footer renders "$X.XX · model"
// alongside in/out/elapsed. Lazily fetches the pricing snapshot on
// the first usage-carrying event we see (one round-trip per Adapter,
// not per turn). nil-Usage events are no-ops.
func (a *Adapter) applyPricing(ev *coretui.Event) {
	if ev == nil || ev.Usage == nil {
		return
	}
	a.mu.Lock()
	fetched := a.pricingFetched
	model := a.pricingModel
	rin := a.pricingIn
	rout := a.pricingOut
	a.mu.Unlock()
	if !fetched {
		info, err := a.client.Pricing(context.TODO(), a.sessionPath)
		a.mu.Lock()
		a.pricingFetched = true // mark fetched even on error so we don't retry every event
		if err == nil {
			a.pricingModel = info.CurrentModel
			if info.Current != nil {
				a.pricingIn = info.Current.InputUSDPerMTok
				a.pricingOut = info.Current.OutputUSDPerMTok
			}
			model = a.pricingModel
			rin = a.pricingIn
			rout = a.pricingOut
		}
		a.mu.Unlock()
	}
	ev.Model = model
	ev.CostUSD = costFromRates(ev.Usage.InputTokens, ev.Usage.OutputTokens, rin, rout)
}

// costFromRates computes $cost = in*inRate/M + out*outRate/M. Zero
// rates → zero cost (free model or unknown rate — same display).
func costFromRates(inTok, outTok int, inRate, outRate float64) float64 {
	const million = 1_000_000.0
	return float64(inTok)/million*inRate + float64(outTok)/million*outRate
}

// ContextWindowSize satisfies coretui.UsageTracker. Returns 0
// (unknown) — would require attach-side surfacing of the model's
// context window cap.
func (a *Adapter) ContextWindowSize() int { return 0 }

// ContextWindowUsed satisfies coretui.UsageTracker. Returns 0
// (unknown) — see ContextWindowSize.
func (a *Adapter) ContextWindowUsed() int { return 0 }

// SessionTurns satisfies coretui.UsageTracker.
func (a *Adapter) SessionTurns() int {
	_, _, turns := a.snapshot()
	return turns
}

// SessionDuration satisfies coretui.UsageTracker. The remote agent
// owns wall-clock; remote attach doesn't surface a session-start
// timestamp. Return 0 (unknown) for v1.
func (a *Adapter) SessionDuration() time.Duration { return 0 }

// ===== Static feeds (Memory / Skills / MCP) =====
//
// These aren't capability interfaces — they're sliced into the
// coretui.Options struct at startup. cmd/core-agent-tui fetches
// them once before the call to coretui.Run and refreshes via the
// operator's /reload slash (which re-queries the server then
// rebuilds the slices).

// FetchMemory returns the remote agent's loaded instruction
// sources for Options.Memory.
func (a *Adapter) FetchMemory(ctx context.Context) []coretui.MemoryFile {
	sources, err := a.client.Memory(ctx, a.sessionPath)
	if err != nil {
		return nil
	}
	out := make([]coretui.MemoryFile, 0, len(sources))
	for _, s := range sources {
		out = append(out, coretui.MemoryFile{
			Path:  s.Path,
			Bytes: int64(s.Size),
		})
	}
	return out
}

// FetchSkills returns the remote agent's registered skills for
// Options.Skills.
func (a *Adapter) FetchSkills(ctx context.Context) []coretui.SkillInfo {
	infos, err := a.client.Skills(ctx, a.sessionPath)
	if err != nil {
		return nil
	}
	out := make([]coretui.SkillInfo, 0, len(infos))
	for _, s := range infos {
		out = append(out, coretui.SkillInfo{
			Name:        s.Name,
			Description: s.Description,
			Source:      "remote",
		})
	}
	return out
}

// FetchMCPServers returns the remote agent's declared MCP servers
// for Options.MCPServers.
func (a *Adapter) FetchMCPServers(ctx context.Context) []coretui.MCPServerInfo {
	info, err := a.client.MCP(ctx, a.sessionPath)
	if err != nil {
		return nil
	}
	out := make([]coretui.MCPServerInfo, 0, len(info.Servers))
	for _, s := range info.Servers {
		ms := coretui.MCPServerInfo{
			Name:      s.Name,
			Transport: s.Transport,
			Connected: s.Status == "running",
			ToolCount: len(s.Tools),
		}
		if len(s.Tools) > 0 {
			ms.Tools = make([]coretui.MCPToolInfo, 0, len(s.Tools))
			for _, t := range s.Tools {
				ms.Tools = append(ms.Tools, coretui.MCPToolInfo{
					Name:        t.Name,
					Description: t.Description,
				})
			}
		}
		out = append(out, ms)
	}
	return out
}

// ===== Permission controller (coretui built-in /permissions slash) =====
//
// Same pattern as PricingController below — coretui's built-in
// /permissions, /allow, /deny slashes type-assert on
// coretui.PermissionController. Bypasses our SlashProvider
// registration when present, so we satisfy it directly.

// SessionApprovals satisfies coretui.PermissionController. Fetches
// the gate's per-session approval log via /perms (which carries
// PermsInfo.Approvals since the v2.1 attach extension) and
// projects to coretui.ApprovalLog. One round-trip per /permissions
// slash invocation — acceptable since the slash is operator-
// initiated, not per-render.
func (a *Adapter) SessionApprovals() []coretui.ApprovalLog {
	info, err := a.client.Perms(context.TODO(), a.sessionPath)
	if err != nil || len(info.Approvals) == 0 {
		return nil
	}
	out := make([]coretui.ApprovalLog, 0, len(info.Approvals))
	for _, ap := range info.Approvals {
		out = append(out, coretui.ApprovalLog{
			Tool:     ap.Tool,
			Key:      ap.Key,
			Decision: ap.Decision,
		})
	}
	return out
}

// AddAllowPatterns satisfies coretui.PermissionController.
func (a *Adapter) AddAllowPatterns(patterns []string) error {
	return a.client.AllowPatterns(context.TODO(), a.sessionPath, patterns)
}

// AddDenyPatterns satisfies coretui.PermissionController.
func (a *Adapter) AddDenyPatterns(patterns []string) error {
	return a.client.DenyPatterns(context.TODO(), a.sessionPath, patterns)
}

// AddBuiltinAllowExtra satisfies coretui.PermissionController. The
// permission "bundles" feature (named approval presets) isn't
// surfaced over attach yet; return a stable error so the operator
// sees "not supported" instead of a silent no-op.
func (a *Adapter) AddBuiltinAllowExtra(bundleName string) error {
	return fmt.Errorf("permission bundles not yet surfaced over attach (bundle: %q)", bundleName)
}

// Reload satisfies coretui.Reloader. Calls the /reload attach
// endpoint (server re-walks AGENTS.md / skills / MCP) and re-
// fetches the static feeds so coretui's /memory /skills /mcp
// modals show the fresh state. Returns Agent=nil — the live agent
// stays the same; only its loaded view changes.
func (a *Adapter) Reload(ctx context.Context) (coretui.ReloadResult, error) {
	resp, err := a.client.Reload(ctx, a.sessionPath)
	if err != nil {
		return coretui.ReloadResult{
			Note: "/reload: " + err.Error(),
		}, nil
	}
	return coretui.ReloadResult{
		Memory:     a.FetchMemory(ctx),
		Skills:     a.FetchSkills(ctx),
		MCPServers: a.FetchMCPServers(ctx),
		Note:       renderReloadResp(resp),
	}, nil
}

// ===== Pricing controller (coretui built-in /pricing slash) =====
//
// coretui's built-in /pricing slash type-asserts on
// coretui.PricingController (different interface from the adapter's
// own /pricing SlashProvider entry). When the built-in fires, our
// own SlashProvider is bypassed, so we satisfy the built-in to
// keep the slash working. Returns a summary string that coretui
// renders as a system message.

// Refresh satisfies coretui.PricingController. Calls the attach
// endpoint /pricing/refresh and projects the response.
func (a *Adapter) Refresh(ctx context.Context) (string, error) {
	resp, err := a.client.RefreshPricing(ctx, a.sessionPath)
	if err != nil {
		return "", err
	}
	return renderRefreshResp(resp), nil
}

// Set satisfies coretui.PricingController. Calls the attach
// endpoint /pricing/set.
func (a *Adapter) Set(modelID string, in, out float64) (string, error) {
	if err := a.client.SetManualPricing(context.TODO(), a.sessionPath, attach.PricingSetRequest{
		Model:            modelID,
		InputUSDPerMTok:  in,
		OutputUSDPerMTok: out,
	}); err != nil {
		return "", err
	}
	return fmt.Sprintf("Applied %s @ $%.2f in / $%.2f out per Mtok.", modelID, in, out), nil
}

// ===== Slash dispatch =====
//
// coretui's SlashProvider / AsyncSlashProviderWithPreamble hooks
// let the host register additional slash commands. The remote
// adapter surfaces /compact, /done, /btw, /subagent (async) plus
// /context, /pricing, /reload, /perms (sync read endpoints).

// SlashCommands satisfies coretui.SlashProvider.
func (a *Adapter) SlashCommands() []coretui.SlashCommandSpec {
	return []coretui.SlashCommandSpec{
		{Name: "compact", Description: "Force a context-window compaction"},
		{Name: "done", Description: "Mark the current task as done; checkpoint the session"},
		{Name: "btw", Description: "Ask a side question without polluting conversation history"},
		{Name: "subagent", Description: "Spawn a background subagent"},
		{Name: "context", Description: "Show context-management snapshot (compactions / checkpoints / subtasks)"},
		{Name: "pricing", Description: "Show current pricing snapshot; sub: refresh / set"},
		{Name: "reload", Description: "Reload memory + skills + MCP from disk"},
		{Name: "perms", Description: "Show permission gate state"},
		{Name: "replan", Description: "Revoke the current plan and force a redraft (plan-first mode only)"},
	}
}

// InvokeSlash satisfies coretui.SlashProvider for the synchronous
// (read-only) slash commands. The async slashes (/compact, /done,
// /btw, /subagent) flow through InvokeSlashAsync below; coretui's
// dispatch checks AsyncSlashProviderWithPreamble first.
func (a *Adapter) InvokeSlash(ctx context.Context, name, args string) (coretui.SlashResult, error) {
	switch name {
	case "context":
		info, err := a.client.Context(ctx, a.sessionPath)
		if err != nil {
			return coretui.SlashResult{}, err
		}
		return coretui.SlashResult{SystemMessage: renderContextInfo(info)}, nil

	case "pricing":
		args = strings.TrimSpace(args)
		switch {
		case args == "":
			info, err := a.client.Pricing(ctx, a.sessionPath)
			if err != nil {
				return coretui.SlashResult{}, err
			}
			return coretui.SlashResult{SystemMessage: renderPricingInfo(info)}, nil
		case args == "refresh":
			resp, err := a.client.RefreshPricing(ctx, a.sessionPath)
			if err != nil {
				return coretui.SlashResult{}, err
			}
			return coretui.SlashResult{SystemMessage: renderRefreshResp(resp)}, nil
		case strings.HasPrefix(args, "set "):
			req, err := parsePricingSet(strings.TrimPrefix(args, "set "))
			if err != nil {
				return coretui.SlashResult{SystemMessage: "/pricing set: " + err.Error()}, nil
			}
			if err := a.client.SetManualPricing(ctx, a.sessionPath, req); err != nil {
				return coretui.SlashResult{}, err
			}
			return coretui.SlashResult{SystemMessage: fmt.Sprintf("/pricing set: applied %s @ $%.2f in / $%.2f out per Mtok", req.Model, req.InputUSDPerMTok, req.OutputUSDPerMTok)}, nil
		default:
			return coretui.SlashResult{SystemMessage: "usage: /pricing | /pricing refresh | /pricing set <model> <input> <output>"}, nil
		}

	case "reload":
		resp, err := a.client.Reload(ctx, a.sessionPath)
		if err != nil {
			return coretui.SlashResult{}, err
		}
		return coretui.SlashResult{SystemMessage: renderReloadResp(resp)}, nil

	case "perms":
		info, err := a.client.Perms(ctx, a.sessionPath)
		if err != nil {
			return coretui.SlashResult{}, err
		}
		return coretui.SlashResult{SystemMessage: renderPermsInfo(info)}, nil

	case "replan":
		resp, err := a.client.Replan(ctx, a.sessionPath, strings.TrimSpace(args))
		if err != nil {
			return coretui.SlashResult{}, err
		}
		msg := resp.Message
		if msg == "" {
			if resp.PlanWasActive {
				msg = "Plan revoked."
			} else {
				msg = "/replan: no active plan."
			}
		}
		return coretui.SlashResult{SystemMessage: msg}, nil
	}
	return coretui.SlashResult{}, fmt.Errorf("unknown slash: %s", name)
}

// InvokeSlashAsync satisfies coretui.AsyncSlashProviderWithPreamble.
// Returns immediately with the preamble string (rendered in-chat
// while the slash dispatches); writes the eventual result to the
// returned channel.
func (a *Adapter) InvokeSlashAsync(ctx context.Context, name, args string) (string, <-chan coretui.SlashResultOrErr) {
	ch := make(chan coretui.SlashResultOrErr, 1)

	var preamble string
	switch name {
	case "compact":
		preamble = "Compacting context…"
	case "done":
		preamble = "Capturing checkpoint summary…"
	case "btw":
		preamble = "Asking the agent…"
	case "subagent":
		preamble = "Spawning subagent…"
	case "new":
		preamble = "Creating new session…"
	default:
		// Sync path: delegate to InvokeSlash off the Update goroutine.
		go func() {
			defer close(ch)
			res, err := a.InvokeSlash(ctx, name, args)
			ch <- coretui.SlashResultOrErr{Res: res, Err: err}
		}()
		return "", ch
	}

	go func() {
		defer close(ch)
		res, err := a.invokeAsyncSlash(ctx, name, args)
		ch <- coretui.SlashResultOrErr{Res: res, Err: err}
	}()
	return preamble, ch
}

// invokeAsyncSlash routes the four async slashes to their attach
// endpoints. The /btw answer renders as a modal (R-CMD-5) so it
// doesn't pollute the persistent chat scrollback; the others
// produce a one-line system message.
func (a *Adapter) invokeAsyncSlash(ctx context.Context, name, args string) (coretui.SlashResult, error) {
	switch name {
	case "compact":
		resp, err := a.client.SlashCompact(ctx, a.sessionPath, strings.TrimSpace(args))
		if err != nil {
			return coretui.SlashResult{}, err
		}
		return coretui.SlashResult{SystemMessage: renderCompactResp(resp)}, nil

	case "done":
		resp, err := a.client.SlashDone(ctx, a.sessionPath, strings.TrimSpace(args))
		if err != nil {
			return coretui.SlashResult{}, err
		}
		return coretui.SlashResult{SystemMessage: renderCheckpointResp(resp)}, nil

	case "btw":
		question := strings.TrimSpace(args)
		if question == "" {
			return coretui.SlashResult{SystemMessage: "/btw: question required"}, nil
		}
		answer, err := a.client.SlashBtw(ctx, a.sessionPath, question)
		if err != nil {
			return coretui.SlashResult{ModalAnswer: &coretui.SideAnswer{Question: question, Err: err}}, nil
		}
		return coretui.SlashResult{ModalAnswer: &coretui.SideAnswer{Question: question, Answer: answer}}, nil

	case "subagent":
		spec, err := parseSubagentSpec(args)
		if err != nil {
			return coretui.SlashResult{SystemMessage: "/subagent: " + err.Error()}, nil
		}
		resp, err := a.client.SlashSubagent(ctx, a.sessionPath, spec)
		if err != nil {
			return coretui.SlashResult{}, err
		}
		return coretui.SlashResult{SystemMessage: fmt.Sprintf("/subagent: spawned %q at %s", resp.Name, resp.StartedAt.Format(time.RFC3339))}, nil

	case "new":
		// /new bypasses the per-session /slash/<name> dispatch and
		// hits the daemon-level POST /sessions endpoint directly —
		// session creation isn't logically scoped to the current
		// session, even though the operator typed the slash inside
		// one. The TUI doesn't tear down + reattach mid-session
		// (that needs coretui-side support, see core-tui#48);
		// instead it shows the new session URL so the operator can
		// relaunch with --new-session or paste the URL into a fresh
		// tab.
		//
		// SystemMessage format: short prefix + URL on its own visual
		// line so even when core-tui truncates long rows
		// (core-tui#49) the URL stays readable. \n inside the
		// SystemMessage renders as a line break in the chat
		// scrollback.
		resp, err := a.client.NewSession(ctx)
		if err != nil {
			return coretui.SlashResult{}, err
		}
		return coretui.SlashResult{SystemMessage: fmt.Sprintf("/new: created %s\n%s", resp.SessionID, resp.URL)}, nil
	}
	return coretui.SlashResult{}, fmt.Errorf("invokeAsyncSlash: unknown slash %s", name)
}

// ===== Render helpers =====

func renderContextInfo(info attach.ContextInfo) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Compactions: %d\nCheckpoints: %d\n", info.Compactions, info.Checkpoints)
	if info.LastTaskNote != "" {
		fmt.Fprintf(&sb, "Last task: %s\n", info.LastTaskNote)
	}
	fmt.Fprintf(&sb, "Total summarized: %d chars\n", info.TotalCharsSummarized)
	fmt.Fprintf(&sb, "Subtask turns: %d  (in:%d out:%d  $%.4f)",
		info.SubtaskTurns, info.SubtaskInputTokens, info.SubtaskOutputTokens, info.SubtaskCostUSD)
	return sb.String()
}

func renderPricingInfo(info attach.PricingInfo) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Source: %s\nKnown models: %d\n", info.Source, info.KnownModels)
	if !info.LastRefresh.IsZero() {
		fmt.Fprintf(&sb, "Last refresh: %s\n", info.LastRefresh.Format(time.RFC3339))
	}
	if info.CurrentModel != "" {
		fmt.Fprintf(&sb, "Current model: %s\n", info.CurrentModel)
	}
	if info.Current != nil {
		fmt.Fprintf(&sb, "Rates: $%.2f in / $%.2f out per Mtok",
			info.Current.InputUSDPerMTok, info.Current.OutputUSDPerMTok)
		if info.Current.Source != "" {
			fmt.Fprintf(&sb, " (source: %s)", info.Current.Source)
		}
	}
	return sb.String()
}

func renderRefreshResp(resp attach.PricingRefreshResponse) string {
	if !resp.Updated {
		if resp.Detail != "" {
			return "Pricing not updated: " + resp.Detail
		}
		return fmt.Sprintf("Pricing unchanged. %d models known.", resp.KnownModels)
	}
	return fmt.Sprintf("Pricing refreshed. %d models known. Last refresh: %s",
		resp.KnownModels, resp.LastRefresh.Format(time.RFC3339))
}

func renderReloadResp(resp attach.ReloadResponse) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Memory: %s\nSkills: %s\nMCP: %s",
		ok(resp.Memory), ok(resp.Skills), ok(resp.MCP))
	if len(resp.Errors) > 0 {
		sb.WriteString("\nErrors:\n  - ")
		sb.WriteString(strings.Join(resp.Errors, "\n  - "))
	}
	return sb.String()
}

func renderPermsInfo(info attach.PermsInfo) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Mode: %s\n", info.Mode)
	if len(info.Allow) > 0 {
		fmt.Fprintf(&sb, "Allow (%d):\n  - %s\n", len(info.Allow), strings.Join(info.Allow, "\n  - "))
	}
	if len(info.Deny) > 0 {
		fmt.Fprintf(&sb, "Deny (%d):\n  - %s", len(info.Deny), strings.Join(info.Deny, "\n  - "))
	}
	return sb.String()
}

func renderCompactResp(resp attach.CompactResponse) string {
	if resp.Skipped {
		return "Compaction skipped (threshold not met)."
	}
	return fmt.Sprintf("Compacted in %dms. Summary: %s",
		resp.DurationMS, truncate(resp.SummaryText, 200))
}

func renderCheckpointResp(resp attach.CheckpointResponse) string {
	if resp.Skipped {
		return "Checkpoint skipped."
	}
	out := fmt.Sprintf("Checkpoint captured in %dms.", resp.DurationMS)
	if resp.TaskNote != "" {
		out += " Task: " + resp.TaskNote
	}
	return out
}

// parsePricingSet parses `<model> <input> <output>` into an attach
// PricingSetRequest.
func parsePricingSet(s string) (attach.PricingSetRequest, error) {
	parts := strings.Fields(s)
	if len(parts) != 3 {
		return attach.PricingSetRequest{}, fmt.Errorf("expected `<model> <input> <output>`")
	}
	var in, out float64
	if _, err := fmt.Sscanf(parts[1], "%f", &in); err != nil {
		return attach.PricingSetRequest{}, fmt.Errorf("input: %w", err)
	}
	if _, err := fmt.Sscanf(parts[2], "%f", &out); err != nil {
		return attach.PricingSetRequest{}, fmt.Errorf("output: %w", err)
	}
	return attach.PricingSetRequest{Model: parts[0], InputUSDPerMTok: in, OutputUSDPerMTok: out}, nil
}

// parseSubagentSpec parses `/subagent <name> <goal>`. Richer specs
// (tools / extras / budgets / explicit SystemPrompt) wait for a
// follow-up; v1 takes name + goal and mirrors the goal into the
// SystemPrompt slot — BackgroundAgentManager requires a non-empty
// SystemPrompt, and using the goal text doubles as a focused
// instruction for the subagent ("Your task: watch the disk for a
// while"). Operators who want richer separation construct the spec
// via the library API instead.
func parseSubagentSpec(args string) (attach.SubagentSpec, error) {
	args = strings.TrimSpace(args)
	if args == "" {
		return attach.SubagentSpec{}, fmt.Errorf("usage: /subagent <name> <goal>")
	}
	parts := strings.SplitN(args, " ", 2)
	if len(parts) < 2 {
		return attach.SubagentSpec{}, fmt.Errorf("usage: /subagent <name> <goal>")
	}
	name := strings.TrimSpace(parts[0])
	goal := strings.TrimSpace(parts[1])
	return attach.SubagentSpec{
		Name:         name,
		Goal:         goal,
		SystemPrompt: "You are an autonomous background subagent. Your task: " + goal,
	}, nil
}

func ok(b bool) string {
	if b {
		return "✓"
	}
	return "✗"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
