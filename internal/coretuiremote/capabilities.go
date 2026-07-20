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
	"sort"
	"strings"
	"sync"
	"time"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/usage"
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
	mu            sync.Mutex
	cached        coretui.Usage
	costUSD       float64
	turns         int
	lastTurn      coretui.Usage // from Overall.PerTurn[-1]; used as fallback in LastTurn()
	lastTurnCost  float64
	lastTurnModel string // keys usage.ContextWindowSizeFor from ContextWindowSize()
	lastFetch     time.Time
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
	// Stash the last per-turn entry so LastTurn() can fall back to
	// authoritative server-side data when the streaming-loop state
	// is empty. In observer / LiveAgent mode the Run/Events loops
	// may not have observed a non-partial event with UsageMetadata
	// (see docs/digest-design.md for the shared-state pathway), so
	// `a.lastTurn` on the Adapter stays zero even after real turns
	// complete — the /usage snapshot always has the truth.
	if n := len(info.PerTurn); n > 0 {
		last := info.PerTurn[n-1]
		a.usage.lastTurn = coretui.Usage{
			InputTokens:  int(last.InputTokens),
			OutputTokens: int(last.OutputTokens),
		}
		a.usage.lastTurnCost = last.CostUSD
		a.usage.lastTurnModel = last.Model
	}
	a.usage.lastFetch = time.Now()
	return a.usage.cached, a.usage.costUSD, a.usage.turns
}

// snapshotLastTurn returns the last per-turn entry from the cached
// /usage snapshot, refreshing the cache when stale. Used by
// LastTurn() as the authoritative fallback when the Run/Events
// streaming loop hasn't populated a.lastTurn.
func (a *Adapter) snapshotLastTurn() (coretui.Usage, float64) {
	// Force a snapshot fetch (also fills lastTurn under the cache mutex).
	_, _, _ = a.snapshot()
	a.usage.mu.Lock()
	defer a.usage.mu.Unlock()
	return a.usage.lastTurn, a.usage.lastTurnCost
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
// per-turn token counts + cost.
//
// Preferred source: the streaming Run/Events loop stashes each
// non-partial event's UsageMetadata into a.lastTurn as they arrive
// (adapter.go). Cost is then computed client-side from the /pricing
// snapshot rates cached in applyPricing.
//
// Fallback: when a.lastTurn is empty — which happens in observer /
// LiveAgent mode for sessions whose SSE events don't surface a non-
// partial UsageMetadata (e.g. when the terminal chunk is Partial:true,
// or when the operator connected after the last such event) — read
// the last per-turn entry from the cached /usage snapshot. That path
// carries authoritative server-side cost (with cache-discount +
// operator pricing overrides already applied) so the /stats slash
// stops rendering "Last turn: 0" for perfectly-fine sessions.
func (a *Adapter) LastTurn() (coretui.Usage, float64) {
	a.mu.Lock()
	live := a.lastTurn
	rin := a.pricingIn
	rout := a.pricingOut
	a.mu.Unlock()
	if live.InputTokens > 0 || live.OutputTokens > 0 {
		return live, costFromRates(live.InputTokens, live.OutputTokens, rin, rout)
	}
	return a.snapshotLastTurn()
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

// ContextWindowSize satisfies coretui.UsageTracker. Resolves the last
// per-turn model from the cached /usage snapshot (or the currently-
// selected pricing model as a pre-first-turn fallback) and looks up
// its cap via usage.ContextWindowSizeFor. Returns 0 when the model is
// unknown or unrecognized — coretui then renders "Context: (unknown)".
//
// Same lookup table the embedded TUI's coreUsageBridge uses (both
// paths flow through pkg/usage), so /stats surfaces identical values
// whether the operator is in the in-process TUI or attached remotely.
func (a *Adapter) ContextWindowSize() int {
	_, _, _ = a.snapshot() // refresh cache; populates lastTurnModel under a.usage.mu
	a.usage.mu.Lock()
	model := a.usage.lastTurnModel
	a.usage.mu.Unlock()
	if model == "" {
		// Pre-first-turn or older daemons that don't stamp per-turn
		// Model: fall back to the pricing snapshot's CurrentModel
		// (populated on the first usage-bearing event via applyPricing).
		a.mu.Lock()
		model = a.pricingModel
		a.mu.Unlock()
	}
	return usage.ContextWindowSizeFor(model)
}

// ContextWindowUsed satisfies coretui.UsageTracker. Approximates the
// current context fill as the most recent turn's input-token count
// (each turn re-sends the full conversation, so input == rolling size
// — matches the in-process Tracker.ContextWindowUsed semantics).
//
// Live streaming state wins over the /usage snapshot fallback (same
// freshness policy as LastTurn): the Run/Events loop stashes each
// non-partial event's usage into a.lastTurn as it arrives, so the
// fill reading catches up on turn-end rather than waiting on the 2s
// snapshot TTL.
func (a *Adapter) ContextWindowUsed() int {
	a.mu.Lock()
	live := a.lastTurn.InputTokens
	a.mu.Unlock()
	if live > 0 {
		return live
	}
	snap, _ := a.snapshotLastTurn()
	return snap.InputTokens
}

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

// ===== Session switch =====
//
// SessionSwitcher backs core-tui's /switch built-in (core-tui #48 / #53).
// Sessions() enumerates via GET /sessions and marks the currently-
// attached row with Current=true; SwitchToSession(id) constructs a
// fresh Adapter pointing at the picked session and hands it back
// inside a SwitchTarget so core-tui can detach + reattach in place.
//
// Detach semantics are core-tui-side: cancelling the outgoing
// Adapter's ctxs closes the local SSE reader; the daemon session
// keeps ticking per its own reattach policy. See the SwitchTarget
// godoc in core-tui for the full contract.

// peerEnumTimeout bounds the per-peer ListSessions call in the
// fan-out so one slow peer doesn't stall the whole picker open.
// Mirrors cmd/core-agent-tui/picker.go's 5s startup budget.
const peerEnumTimeout = 5 * time.Second

// Sessions satisfies coretui.SessionSwitcher. Enumerates sessions
// on the current daemon, and (when a ClientFactory is wired via
// NewWithClientFactory) parallel-fans across peers advertised on
// GET /peers to include their sessions too. Local rows come first;
// peer rows follow, each tagged in Display with the peer origin
// so operators can tell which daemon they're picking. Currently-
// attached row marked Current=true.
//
// On local enumeration failure returns nil (picker then renders
// "no sessions advertised by the agent"). Peer failures are
// silently dropped per row so a single unhappy peer doesn't take
// the whole enumeration down. Single-daemon adapters (no factory)
// skip peer enumeration entirely — same behavior as v0.10.x.
//
// Side effect: repopulates the endpointByID map so
// SwitchToSession can decide whether a picked session is local
// (empty / missing → use a.client) or peer (non-empty endpoint →
// build a fresh Client via clientFactory).
func (a *Adapter) Sessions() []coretui.SessionInfo {
	descs, err := a.client.ListSessions(context.TODO())
	if err != nil {
		return nil
	}
	curSID := currentSessionID(a.sessionPath)

	// Fresh endpoint map per enumeration — stale entries from a prior
	// Sessions() call must not survive a peer disappearing from
	// GET /peers.
	newMap := make(map[string]string, len(descs))
	out := make([]coretui.SessionInfo, 0, len(descs))
	for _, d := range descs {
		display := d.SessionID
		if d.App != "" {
			display = d.App + "/" + d.SessionID
		}
		out = append(out, coretui.SessionInfo{
			ID:      d.SessionID,
			Display: display,
			Current: d.SessionID == curSID,
		})
		newMap[d.SessionID] = "" // "" = local
	}

	// Peer fan-out only when the factory is wired (multi-daemon
	// mode). Bare New adapters stay single-daemon.
	if a.clientFactory != nil {
		peers, perr := a.client.ListPeers(context.TODO())
		if perr == nil && len(peers) > 0 {
			peerRows := a.enumeratePeers(peers)
			for _, row := range peerRows {
				out = append(out, row.info)
				newMap[row.info.ID] = row.endpoint
			}
		}
	}

	a.mu.Lock()
	a.endpointByID = newMap
	a.mu.Unlock()

	return out
}

// peerRow is one enumerated peer session plus the endpoint that
// serves it — internal-only, so the endpoint→ID cache stays in
// step with what Sessions() actually returned.
type peerRow struct {
	info     coretui.SessionInfo
	endpoint string
}

// enumeratePeers parallel-fans across peers, calling ListSessions
// on each via a fresh Client from clientFactory. Per-peer timeout
// bounds the wait; peers that error (auth failure, network drop,
// no factory support for their scheme, etc.) are silently dropped.
// Sorted by peer name for deterministic picker ordering across
// enumerations.
func (a *Adapter) enumeratePeers(peers []attachclient.PeerDescriptor) []peerRow {
	type result struct {
		peer attachclient.PeerDescriptor
		rows []peerRow
	}
	results := make(chan result, len(peers))
	for _, p := range peers {
		go func(p attachclient.PeerDescriptor) {
			out := result{peer: p}
			if p.Endpoint == "" {
				results <- out
				return
			}
			c, err := a.clientFactory(p.Endpoint)
			if err != nil {
				results <- out
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), peerEnumTimeout)
			defer cancel()
			descs, err := c.ListSessions(ctx)
			if err != nil {
				results <- out
				return
			}
			for _, d := range descs {
				display := d.SessionID
				if d.App != "" {
					display = d.App + "/" + d.SessionID
				}
				peerLabel := p.Name
				if peerLabel == "" {
					peerLabel = p.Endpoint
				}
				out.rows = append(out.rows, peerRow{
					info: coretui.SessionInfo{
						ID:          d.SessionID,
						Display:     "[peer:" + peerLabel + "] " + display,
						Description: p.Endpoint,
					},
					endpoint: p.Endpoint,
				})
			}
			results <- out
		}(p)
	}
	collected := make([]result, 0, len(peers))
	for i := 0; i < len(peers); i++ {
		collected = append(collected, <-results)
	}
	// Deterministic order across enumerations — sort by peer name
	// (fallback endpoint), then flatten. Without this the picker
	// order shifts run-to-run and (current) marker jumps around.
	sort.Slice(collected, func(i, j int) bool {
		ni, nj := collected[i].peer.Name, collected[j].peer.Name
		if ni == "" {
			ni = collected[i].peer.Endpoint
		}
		if nj == "" {
			nj = collected[j].peer.Endpoint
		}
		return ni < nj
	})
	var flat []peerRow
	for _, r := range collected {
		flat = append(flat, r.rows...)
	}
	return flat
}

// SwitchToSession satisfies coretui.SessionSwitcher. Local targets
// (empty / missing endpoint in the cache populated by Sessions())
// construct a fresh Adapter against the same client; peer targets
// (non-empty endpoint) build a fresh Client via clientFactory and
// return an Adapter pointing at that peer's session. In both cases
// the returned Adapter inherits this Adapter's clientFactory so
// the operator can hop again from the new session.
//
// Direct-jump operator-typed IDs that Sessions() hasn't seen fall
// through the local path (resolveSessionPath handles the "unknown
// sid" case). Cross-daemon direct-jumps by bare sid aren't
// supported — the operator uses /attach <url> <sid> for that.
func (a *Adapter) SwitchToSession(sessionID string) (coretui.SwitchTarget, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return coretui.SwitchTarget{}, fmt.Errorf("SwitchToSession: empty session ID")
	}
	if sessionID == currentSessionID(a.sessionPath) {
		// Already attached — no-op switch. core-tui's picker also
		// no-ops on the Current row so this is mostly a direct-jump-
		// typing safety.
		return coretui.SwitchTarget{
			Agent: a,
			Note:  "/switch: already attached to " + sessionID,
		}, nil
	}

	// Peer targets: use the endpoint the last Sessions() enumeration
	// recorded, construct a fresh Client via the factory, wrap in a
	// new Adapter that inherits the factory (so the operator can hop
	// again from the peer).
	a.mu.Lock()
	endpoint := a.endpointByID[sessionID]
	a.mu.Unlock()
	if endpoint != "" {
		if a.clientFactory == nil {
			// Shouldn't happen (endpoints only populate when factory
			// is wired), but defensive against internal invariant
			// drift.
			return coretui.SwitchTarget{}, fmt.Errorf("SwitchToSession: peer target %q but clientFactory not wired", sessionID)
		}
		peerClient, err := a.clientFactory(endpoint)
		if err != nil {
			return coretui.SwitchTarget{}, fmt.Errorf("clientFactory(%s): %w", endpoint, err)
		}
		newPath, err := resolvePathOnClient(peerClient, sessionID)
		if err != nil {
			return coretui.SwitchTarget{}, err
		}
		next := NewWithClientFactory(peerClient, newPath, a.clientFactory)
		next.SetBrander(a.branderFn())
		return a.buildSwitchTarget(next, newPath,
			"Attached to peer session "+sessionID+" ("+endpoint+")"), nil
	}

	newPath, err := a.resolveSessionPath(sessionID)
	if err != nil {
		return coretui.SwitchTarget{}, err
	}
	next := NewWithClientFactory(a.client, newPath, a.clientFactory)
	next.SetBrander(a.branderFn())
	return a.buildSwitchTarget(next, newPath, "Attached to session "+sessionID), nil
}

// branderFn returns the brander closure under lock so SwitchToSession
// can safely propagate it to the incoming Adapter without racing with
// a concurrent SetBrander from the host. Nil is a valid return.
func (a *Adapter) branderFn() func(string) *coretui.Branding {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.brander
}

// buildSwitchTarget assembles the full coretui.SwitchTarget for a
// /switch. Populates UsageTracker (next itself implements the
// interface, and its cache is scoped to its own sessionPath),
// Memory / Skills / MCPServers (fresh remote fetches against the
// incoming session — mirrors the initial-attach wiring in
// cmd/core-agent-tui/main.go), and Branding (via the brander closure
// if the host wired one). Without this population, core-tui's
// applySwitchTarget leaves the corresponding Options fields pointed
// at the outgoing session's state (issue #274).
//
// Fetch failures inside the FetchX helpers already degrade to nil
// slices; a nil Memory/Skills/MCPServers slice on the SwitchTarget
// means "don't replace" per the field godoc, so a transient network
// blip during /switch keeps the outgoing snapshot rather than
// clearing it. That's the safe fallback.
func (a *Adapter) buildSwitchTarget(next *Adapter, newPath, note string) coretui.SwitchTarget {
	ctx := context.TODO()
	tgt := coretui.SwitchTarget{
		Agent:        next,
		UsageTracker: next,
		Memory:       next.FetchMemory(ctx),
		Skills:       next.FetchSkills(ctx),
		MCPServers:   next.FetchMCPServers(ctx),
		Note:         note,
	}
	if fn := a.branderFn(); fn != nil {
		tgt.Branding = fn(newPath)
	}
	return tgt
}

// currentSessionID extracts the trailing sessionID from a session
// path. The path is one of:
//
//	/sessions/<sid>              — shortcut form
//	/sessions/<app>/<sid>        — qualified
//
// A malformed path yields "" so the caller's compare-and-mark logic
// simply leaves Current=false on every row (the picker still opens
// and switching still works — only the visual marker is lost).
func currentSessionID(sessionPath string) string {
	rest := strings.TrimPrefix(sessionPath, "/sessions/")
	if idx := strings.LastIndex(rest, "/"); idx >= 0 {
		return rest[idx+1:]
	}
	return rest
}

// resolveSessionPath maps a bare sessionID to the App-qualified path
// the current daemon expects. Thin wrapper over resolvePathOnClient
// for the common case; peer-daemon path resolution uses
// resolvePathOnClient directly with a fresh client.
func (a *Adapter) resolveSessionPath(sessionID string) (string, error) {
	return resolvePathOnClient(a.client, sessionID)
}

// resolvePathOnClient enumerates GET /sessions on client and picks
// the first row whose SessionID matches, returning the App-qualified
// path form (or shortcut form when App is empty). Falls back to
// the shortcut form when the enumeration doesn't find a match, so
// a stale operator-typed ID surfaces via a downstream HTTP error
// rather than an opaque enumeration miss here.
//
// Extracted so SwitchToSession (peer branch) + /attach can share
// the resolution against arbitrary daemons — the pre-#246 form was
// a method on Adapter and only spoke to a.client.
func resolvePathOnClient(client *attachclient.Client, sessionID string) (string, error) {
	descs, err := client.ListSessions(context.TODO())
	if err != nil {
		return "", fmt.Errorf("ListSessions: %w", err)
	}
	for _, d := range descs {
		if d.SessionID != sessionID {
			continue
		}
		if d.App == "" {
			return "/sessions/" + d.SessionID, nil
		}
		return "/sessions/" + d.App + "/" + d.SessionID, nil
	}
	return "/sessions/" + sessionID, nil
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
		{Name: "usage", Description: "Show cache-hit attribution + per-turn cost breakdown"},
		{Name: "pricing", Description: "Show current pricing snapshot; sub: refresh / set"},
		{Name: "reload", Description: "Reload memory + skills + MCP from disk"},
		{Name: "perms", Description: "Show permission gate state"},
		{Name: "replan", Description: "Revoke the current plan and force a redraft (plan-first mode only)"},
		{Name: "new", Description: "Create a fresh daemon session and attach in place (companion to core-tui's /switch)"},
		{Name: "attach", Description: "Attach to another daemon: /attach <url> lists that daemon's sessions; /attach <url> <sid> direct-jumps in place. Escape hatch when GET /peers is empty (issue #246)."},
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

	case "usage":
		info, err := a.client.Usage(ctx, a.sessionPath)
		if err != nil {
			return coretui.SlashResult{}, err
		}
		return coretui.SlashResult{SystemMessage: attach.RenderUsage(info)}, nil

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
	case "attach":
		preamble = "Contacting daemon…"
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
		// one. As of core-tui v0.10.0 (issue #48) we can now detach
		// + reattach in place: return SwitchTo carrying a fresh
		// Adapter pointing at the new session, and core-tui swaps
		// the chat over on the same tick.
		resp, err := a.client.NewSession(ctx)
		if err != nil {
			return coretui.SlashResult{}, err
		}
		newPath := "/sessions/" + resp.SessionID
		if resp.AppName != "" {
			newPath = "/sessions/" + resp.AppName + "/" + resp.SessionID
		}
		// Preserve clientFactory (nil in single-daemon mode) so the
		// operator can hop again from the new session without losing
		// multi-daemon capabilities they had before /new.
		next := NewWithClientFactory(a.client, newPath, a.clientFactory)
		return coretui.SlashResult{
			SwitchTo: &coretui.SwitchTarget{
				Agent: next,
				Note:  fmt.Sprintf("Attached to new session %s (%s)", resp.SessionID, resp.URL),
			},
		}, nil

	case "attach":
		// /attach <url>          → enumerate that daemon's sessions
		//                          into a system message; operator
		//                          picks a sid and reissues.
		// /attach <url> <sid>    → direct-jump: build a fresh Adapter
		//                          against the peer daemon's sid path.
		//
		// Escape hatch (issue #246) for when GET /peers is empty on
		// the current daemon or the operator wants to reach an
		// unregistered daemon. Requires a wired clientFactory —
		// single-daemon adapters error cleanly.
		return a.dispatchAttach(ctx, args)
	}
	return coretui.SlashResult{}, fmt.Errorf("invokeAsyncSlash: unknown slash %s", name)
}

// dispatchAttach handles /attach <url> [<sid>] (issue #246).
//
//	/attach <url>          → contact daemon at <url>, enumerate its
//	                         sessions, return a system message listing
//	                         them. No switch.
//	/attach <url> <sid>    → direct-jump: construct a fresh Adapter
//	                         against <url>/sessions/<sid> (App-resolved
//	                         via ListSessions on the peer) and return
//	                         a SwitchTarget.
//
// Requires clientFactory to be wired (NewWithClientFactory).
// Single-daemon adapters get a friendly error pointing at #246.
func (a *Adapter) dispatchAttach(ctx context.Context, args string) (coretui.SlashResult, error) {
	if a.clientFactory == nil {
		return coretui.SlashResult{
			SystemMessage: "/attach: cross-daemon switch requires multi-daemon mode (host built with NewWithClientFactory — see issue #246). Standard core-agent-tui builds enable this automatically.",
		}, nil
	}
	parts := strings.Fields(args)
	if len(parts) == 0 {
		return coretui.SlashResult{
			SystemMessage: "/attach: usage — /attach <daemon-url> [<session-id>]",
		}, nil
	}
	rawURL := parts[0]
	// Parse to fail-fast on malformed URLs before we hit the network.
	if _, err := attachclient.ParseURL(rawURL); err != nil {
		return coretui.SlashResult{SystemMessage: "/attach: parse URL: " + err.Error()}, nil
	}
	peerClient, err := a.clientFactory(rawURL)
	if err != nil {
		return coretui.SlashResult{SystemMessage: "/attach: clientFactory: " + err.Error()}, nil
	}

	// Bare form: enumerate the peer's sessions into a system message.
	// Operator picks a sid and re-runs `/attach <url> <sid>`. Chose
	// this over "attach to first session" because the operator may
	// not know what's on the peer; a listing makes the choice explicit.
	if len(parts) == 1 {
		enumCtx, cancel := context.WithTimeout(ctx, peerEnumTimeout)
		defer cancel()
		descs, err := peerClient.ListSessions(enumCtx)
		if err != nil {
			return coretui.SlashResult{SystemMessage: "/attach: ListSessions on " + rawURL + ": " + err.Error()}, nil
		}
		if len(descs) == 0 {
			return coretui.SlashResult{
				SystemMessage: "/attach: " + rawURL + " has no sessions. Post one via `curl -X POST " + rawURL + "/sessions` first, then rerun.",
			}, nil
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "/attach: %d session(s) on %s — re-run /attach <url> <sid> to jump:\n", len(descs), rawURL)
		for _, d := range descs {
			if d.App != "" {
				fmt.Fprintf(&sb, "  • %s/%s\n", d.App, d.SessionID)
			} else {
				fmt.Fprintf(&sb, "  • %s\n", d.SessionID)
			}
		}
		return coretui.SlashResult{SystemMessage: strings.TrimRight(sb.String(), "\n")}, nil
	}

	// /attach <url> <sid>: direct-jump.
	sid := parts[1]
	newPath, err := resolvePathOnClient(peerClient, sid)
	if err != nil {
		return coretui.SlashResult{SystemMessage: "/attach: resolve path on " + rawURL + ": " + err.Error()}, nil
	}
	next := NewWithClientFactory(peerClient, newPath, a.clientFactory)
	return coretui.SlashResult{
		SwitchTo: &coretui.SwitchTarget{
			Agent: next,
			Note:  "Attached to " + rawURL + " session " + sid,
		},
	}, nil
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
		if info.Current.CachedUSDPerMTok > 0 {
			fmt.Fprintf(&sb, " / $%.4f cache-read", info.Current.CachedUSDPerMTok)
		}
		if !info.Current.UpdatedAt.IsZero() {
			fmt.Fprintf(&sb, " (updated %s)", info.Current.UpdatedAt.Format("2006-01-02"))
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
