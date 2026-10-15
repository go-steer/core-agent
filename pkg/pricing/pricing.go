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

// Package pricing is core-agent's name for core-models' shared price
// catalog, github.com/go-steer/core-models/pricing. It used to carry its
// own LiteLLM-generated table, regenerator and layered catalog; mast
// carried another, and the two drifted. Both products now price from
// the one catalog core-models ships and regenerates weekly, and this
// package re-exports it so existing callers (pkg/usage, pkg/compose,
// embedders driving Refresh or building a *Catalog per tenant) keep
// compiling.
//
// Every type here is an alias, so a value from this package IS the
// core-models value: nothing is copied or converted at the boundary.
//
// Lookup chain (first exact match wins, then longest prefix):
//
//  1. cfg.Model.Pricing[name]     — operator override in .agents/config.json.
//  2. .agents/pricing.json        — project-local additions.
//  3. ~/.core-agent/pricing.json  — `manual` (/pricing set) and
//     `external` (the daily LiteLLM refresh).
//  4. builtin                     — core-models' compiled-in table.
//  5. declared                    — rates provider profiles declare
//     (models[].rates), for models no catalog covers.
//
// Catalog.LookupFor(backend, model) consults "<backend>/<model>" before
// the bare id in each layer, so a model billed differently on two
// backends (Claude first-party vs on Vertex AI, OpenAI's and xAI's
// models, which have no bare rows) gets its own rate. Catalog.Lookup is
// LookupFor with no backend. See the core-models package docs for the
// exact order.
package pricing

import (
	"context"

	cm "github.com/go-steer/core-models/pricing"
	"github.com/go-steer/core-models/profile"
)

// Rate and catalog types.
type (
	Rates            = cm.Rates
	LongContext      = cm.LongContext
	ModelRates       = cm.ModelRates
	Catalog          = cm.Catalog
	CountByLayer     = cm.CountByLayer
	Options          = cm.Options
	ProjectFile      = cm.ProjectFile
	UserFile         = cm.UserFile
	ExternalSource   = cm.ExternalSource
	ManualSection    = cm.ManualSection
	RefreshOptions   = cm.RefreshOptions
	RefreshOutcome   = cm.RefreshOutcome
	IntroductoryRate = cm.IntroductoryRate
	Verdict          = cm.Verdict
)

// Source names LookupWithSource reports.
const (
	SourceCfgOverride  = cm.SourceCfgOverride
	SourceProjectFile  = cm.SourceProjectFile
	SourceUserManual   = cm.SourceUserManual
	SourceUserExternal = cm.SourceUserExternal
	SourceBuiltin      = cm.SourceBuiltin
	SourceDeclared     = cm.SourceDeclared
)

// File and refresh constants.
const (
	SchemaVersion              = cm.SchemaVersion
	ProjectFileName            = cm.ProjectFileName
	UserFileName               = cm.UserFileName
	DefaultRefreshSource       = cm.DefaultRefreshSource
	DefaultRefreshInterval     = cm.DefaultRefreshInterval
	DefaultMaxRefreshBodyBytes = cm.DefaultMaxRefreshBodyBytes
	// ExternalFormat is the ExternalSource.Format a refresh writes. A
	// cache in an older format (written by core-models v0.13.2 or
	// earlier) is re-qualified on load and refetched in full on the next
	// refresh.
	ExternalFormat = cm.ExternalFormat
)

// NewCatalog reads every configured source and returns the merged catalog.
func NewCatalog(opts Options) (*Catalog, error) { return cm.NewCatalog(opts) }

// LoadProjectFile reads <agentsDir>/pricing.json; a missing file is not an error.
func LoadProjectFile(agentsDir string) (*ProjectFile, error) { return cm.LoadProjectFile(agentsDir) }

// LoadUserFile reads <userHome>/pricing.json; a missing file is not an error.
func LoadUserFile(userHome string) (*UserFile, error) { return cm.LoadUserFile(userHome) }

// SaveUserFile writes <userHome>/pricing.json atomically.
func SaveUserFile(userHome string, uf *UserFile) error { return cm.SaveUserFile(userHome, uf) }

// Refresh fetches LiteLLM's catalog into the user file's external section.
func Refresh(ctx context.Context, userHome string, opts RefreshOptions) (RefreshOutcome, error) {
	return cm.Refresh(ctx, userHome, opts)
}

// Builtin returns a copy of the compiled-in table, bare model ids only.
func Builtin() map[string]Rates { return cm.Builtin() }

// BuiltinByBackend returns a copy of the compiled-in (backend, model)
// table, keyed "<backend>/<model>".
func BuiltinByBackend() map[string]Rates { return cm.BuiltinByBackend() }

// BuiltinContextWindow returns the compiled-in max input window for a
// lowercase model id.
func BuiltinContextWindow(modelID string) (int, bool) { return cm.BuiltinContextWindow(modelID) }

// BuiltinContextWindows returns a copy of the whole window table.
func BuiltinContextWindows() map[string]int { return cm.BuiltinContextWindows() }

// DeclaredFromProfiles collects the rates provider profiles declare into
// Options.Declared, keyed "<backend>/<model>".
func DeclaredFromProfiles(ps []profile.Profile) map[string]ModelRates {
	return cm.DeclaredFromProfiles(ps)
}

// IntroductoryRates lists the built-in rates known to be introductory,
// with the date each is due to end.
func IntroductoryRates() []IntroductoryRate { return cm.IntroductoryRates() }
