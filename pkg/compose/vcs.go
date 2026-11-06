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

package compose

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"google.golang.org/adk/v2/session"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
	"github.com/go-steer/core-agent/v3/pkg/tools/vcs"
)

// BuildVCSTools constructs the vcs_* built-in tool suite when cfg.VCS.Enabled()
// is true, merges extraDisabled with cfg.Tools.Disable, and updates
// gate.SetRegisteredTools so the gate's tool catalog reflects the registered
// vcs_* tools (#1490).
func BuildVCSTools(cfg *config.Config, gate *permissions.Gate, cwd string, extraDisabled []string, opts ...vcs.Option) ([]adktool.Tool, error) {
	if cfg == nil || !cfg.VCS.Enabled() {
		return nil, nil
	}
	effective := cfg
	if len(extraDisabled) > 0 {
		cp := *cfg
		combined := make([]string, 0, len(cfg.Tools.Disable)+len(extraDisabled))
		combined = append(combined, cfg.Tools.Disable...)
		for _, name := range extraDisabled {
			name = strings.TrimSpace(name)
			if name != "" && !slices.Contains(combined, name) {
				combined = append(combined, name)
			}
		}
		cp.Tools.Disable = combined
		effective = &cp
	}
	built, err := vcs.Build(effective, gate, cwd, opts...)
	if err != nil {
		return nil, err
	}
	if gate != nil && len(built) > 0 {
		reg := maps.Clone(gate.RegisteredTools())
		if reg == nil {
			reg = make(map[string]bool, len(built))
		}
		for _, t := range built {
			if t != nil {
				reg[t.Name()] = true
			}
		}
		gate.SetRegisteredTools(reg)
	}
	return built, nil
}

// FormatVCSStartupLine renders the startup summary line for the vcs subsystem
// when cfg.VCS.Enabled() is true, or returns "" when VCS is not configured.
func FormatVCSStartupLine(cfg *config.Config) string {
	if cfg == nil || !cfg.VCS.Enabled() {
		return ""
	}
	forges := make([]string, 0, len(cfg.VCS.Forges))
	for _, f := range cfg.VCS.Forges {
		host := f.Host
		if host == "" {
			host = f.Provider
		}
		forges = append(forges, fmt.Sprintf("%s=%s/%s", f.Name, f.Provider, host))
	}
	sort.Strings(forges)

	repos := make([]string, 0, len(cfg.VCS.Repositories))
	for _, r := range cfg.VCS.Repositories {
		base := r.BaseBranch
		if base == "" {
			base = "main"
		}
		repos = append(repos, fmt.Sprintf("%s@%s[role=%s,base=%s]", r.Repository, r.Forge, r.Role, base))
	}
	sort.Strings(repos)

	prefix := strings.TrimSpace(cfg.VCS.BranchPrefix)
	if prefix == "" {
		prefix = "agent/"
	}
	return fmt.Sprintf("vcs: %d forge(s) (%s), %d repo(s) (%s), branch_prefix=%q",
		len(forges), strings.Join(forges, ", "),
		len(repos), strings.Join(repos, ", "),
		prefix)
}

// VCSEventSink returns a vcs.EventSink that appends typed vcs.published and
// vcs.proposed audit events to the active session in the durable eventlog when
// getHandle returns a non-nil *eventlog.Handle (#1490).
func VCSEventSink(getHandle func() *eventlog.Handle) vcs.EventSink {
	return func(ctx context.Context, ev vcs.Event) {
		if getHandle == nil {
			return
		}
		h := getHandle()
		if h == nil || h.Service == nil {
			return
		}
		appName := ev.AppName
		if appName == "" {
			appName = "core-agent"
		}
		userID := ev.UserID
		if userID == "" {
			userID = "local"
		}
		sessionID := ev.SessionID
		if sessionID == "" {
			return
		}
		if ctx == nil {
			ctx = context.Background()
		}
		getResp, err := h.Service.Get(ctx, &session.GetRequest{
			AppName:         appName,
			UserID:          userID,
			SessionID:       sessionID,
			NumRecentEvents: 1,
		})
		if err != nil || getResp == nil || getResp.Session == nil {
			return
		}
		var text string
		switch ev.Kind {
		case vcs.EventKindPublished:
			text = fmt.Sprintf("vcs.published %s/%s branch=%s revision=%s", ev.Forge, ev.Repository, ev.Branch, ev.Revision)
		case vcs.EventKindProposed:
			text = fmt.Sprintf("vcs.proposed %s/%s #%d %s", ev.Forge, ev.Repository, ev.Number, ev.URL)
		default:
			text = fmt.Sprintf("%s %s/%s", ev.Kind, ev.Forge, ev.Repository)
		}
		adkEv := session.NewEvent(ctx, "vcs")
		adkEv.Author = "system"
		adkEv.Content = genai.NewContentFromText(text, genai.RoleUser)
		adkEv.CustomMetadata = map[string]any{
			"core_agent_event_kind": ev.Kind,
			"vcs_event": map[string]any{
				"kind":       ev.Kind,
				"forge":      ev.Forge,
				"repository": ev.Repository,
				"branch":     ev.Branch,
				"revision":   ev.Revision,
				"number":     ev.Number,
				"url":        ev.URL,
			},
		}
		_ = h.Service.AppendEvent(ctx, getResp.Session, adkEv)
	}
}
