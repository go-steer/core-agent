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

package main

import (
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/internal/pricing"
)

// describeRefresh renders a one-line summary of a pricing-refresh
// outcome to w. Surfaces the four distinct shapes operators care
// about: fresh write (model count), 304-not-modified, skipped
// (cache still within MinInterval), network failure (cache age +
// error so the operator knows to expect stale rates).
func describeRefresh(w io.Writer, out pricing.RefreshOutcome) {
	switch {
	case out.NetworkFailed:
		if out.StaleAge > 0 {
			fmt.Fprintf(w, "core-agent: pricing refresh: using %s-old cache; network: %v\n",
				out.StaleAge.Round(time.Hour), out.NetworkError)
			return
		}
		fmt.Fprintf(w, "core-agent: pricing refresh: %v (no cache; rates will fall back to built-in table)\n", out.NetworkError)
	case out.Skipped:
		// Quiet path — the refresh was a no-op because the cache
		// is still within MinInterval. Don't bother the operator.
		return
	case out.NotModified:
		// Server confirmed cache is current. Also quiet.
		return
	default:
		fmt.Fprintf(w, "core-agent: pricing refresh: updated %d models from upstream\n", out.ModelCount)
	}
}

// cfgToCatalogOverride translates config.PricingMap (the JSON-tagged
// per-model rate map operators put under model.pricing) into the
// internal/pricing wire shape. nil-safe; an empty map means "no
// cfg override, fall through to the file + builtin layers".
func cfgToCatalogOverride(m config.PricingMap) map[string]pricing.ModelRates {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]pricing.ModelRates, len(m))
	for k, v := range m {
		out[k] = pricing.ModelRates{
			InputPerMTok:  v.InputPerMTok,
			OutputPerMTok: v.OutputPerMTok,
		}
	}
	return out
}

// .agents/config.json persistence helpers used by the TUI's
// /permissions, /allow, /deny, /model, and "always allow"
// flows. Lifted from cogo's internal/tui/program.go and lowered
// into the cmd/core-agent layer because the TUI itself should not
// need to know about .agents/ on-disk layout — main.go has the
// agentsDir resolution and feeds these as closures via
// tui.Options.

// appendPathScope adds pattern to .agents/config.json's
// path_scope.allow list and rewrites the file atomically. If the
// file doesn't exist yet it is created with defaults so the
// addition has somewhere to live.
func appendPathScope(agentsDir, pattern string) error {
	cfg, err := config.Load(agentsDir)
	if err != nil {
		return err
	}
	for _, existing := range cfg.PathScope.Allow {
		if existing == pattern {
			return nil
		}
	}
	cfg.PathScope.Allow = append(cfg.PathScope.Allow, pattern)
	return config.Save(filepath.Join(agentsDir, config.ConfigFileName), cfg)
}

// appendPermissionsAllow adds one or more patterns to
// .agents/config.json's permissions.allow list. Idempotent —
// duplicate patterns are skipped silently so /permissions can be
// re-run without growing the config file.
func appendPermissionsAllow(agentsDir string, patterns []string) error {
	cfg, err := config.Load(agentsDir)
	if err != nil {
		return err
	}
	existing := make(map[string]bool, len(cfg.Permissions.Allow))
	for _, p := range cfg.Permissions.Allow {
		existing[p] = true
	}
	for _, p := range patterns {
		if existing[p] {
			continue
		}
		cfg.Permissions.Allow = append(cfg.Permissions.Allow, p)
		existing[p] = true
	}
	return config.Save(filepath.Join(agentsDir, config.ConfigFileName), cfg)
}

// appendPermissionsDeny mirrors appendPermissionsAllow for the deny
// list. Idempotent.
func appendPermissionsDeny(agentsDir string, patterns []string) error {
	cfg, err := config.Load(agentsDir)
	if err != nil {
		return err
	}
	existing := make(map[string]bool, len(cfg.Permissions.Deny))
	for _, p := range cfg.Permissions.Deny {
		existing[p] = true
	}
	for _, p := range patterns {
		if existing[p] {
			continue
		}
		cfg.Permissions.Deny = append(cfg.Permissions.Deny, p)
		existing[p] = true
	}
	return config.Save(filepath.Join(agentsDir, config.ConfigFileName), cfg)
}

// appendBuiltinAllowExtra adds name to .agents/config.json's
// permissions.builtin_allow_extras list. Idempotent — re-enabling a
// bundle that's already on is a no-op. Validation against the
// bundle catalog (permissions.KnownBundles) happens in the TUI
// before this is called, so an invalid name never reaches disk.
func appendBuiltinAllowExtra(agentsDir, name string) error {
	cfg, err := config.Load(agentsDir)
	if err != nil {
		return err
	}
	for _, existing := range cfg.Permissions.BuiltinAllowExtras {
		if existing == name {
			return nil
		}
	}
	cfg.Permissions.BuiltinAllowExtras = append(cfg.Permissions.BuiltinAllowExtras, name)
	return config.Save(filepath.Join(agentsDir, config.ConfigFileName), cfg)
}

// persistModelChoice writes the new model name to
// .agents/config.json so /model survives across runs. Caller is
// responsible for first invoking the in-memory rebuild via
// tui.Options.RebuildAgent — this is purely the disk side.
func persistModelChoice(agentsDir, modelID string) error {
	cfg, err := config.Load(agentsDir)
	if err != nil {
		return err
	}
	cfg.Model.Name = modelID
	return config.Save(filepath.Join(agentsDir, config.ConfigFileName), cfg)
}
