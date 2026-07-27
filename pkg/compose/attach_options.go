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
	"os"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// AttachOptions is the resolved attach-listener configuration a host
// feeds into its attach.Options / registration wiring. One value
// bundles what the bundled CLI exposes as eleven `--attach-*` flags
// plus the config file's `attach` block; library consumers usually
// fill it straight from BuildAttachOptions and overlay their own
// flag/env precedence on top (the bundled CLI keeps its CLI-beats-
// config overlay in package main, where flag.Visit lives).
type AttachOptions struct {
	Listen           string
	UnixSocket       string
	TLSCert          string
	TLSKey           string
	ClientCA         string
	TokenEnv         string
	ReadOnly         bool
	PeerHub          bool
	RegisterTo       string
	RegisterName     string
	RegisterEndpoint string
	// UI enables the /ui/* route on the attach listener serving the
	// mast-web operator UI. Uses the embedded bundle from
	// internal/webui (populated by dev/tools/fetch-mast-web at build
	// time) unless UIDir overrides with a local directory. CLI-only
	// in the bundled binary — the config file's attach block has no
	// counterpart, so BuildAttachOptions leaves both zero.
	UI    bool
	UIDir string
}

// BuildAttachOptions translates the config file's `attach` block into
// an AttachOptions value, expanding ${ENV_VAR} references in every
// string field so paths and addresses can be parameterized per
// deployment ("$RUNTIME_DIR/agent.sock"). This is the config half of
// the bundled CLI's flag-vs-config merge; hosts with their own flag
// surface apply whatever precedence they want on top.
func BuildAttachOptions(cfg config.AttachConfig) AttachOptions {
	return AttachOptions{
		Listen:           os.ExpandEnv(cfg.Listen),
		UnixSocket:       os.ExpandEnv(cfg.UnixSocket),
		TLSCert:          os.ExpandEnv(cfg.TLSCert),
		TLSKey:           os.ExpandEnv(cfg.TLSKey),
		ClientCA:         os.ExpandEnv(cfg.ClientCA),
		TokenEnv:         os.ExpandEnv(cfg.TokenEnv),
		ReadOnly:         cfg.ReadOnly,
		PeerHub:          cfg.PeerHub,
		RegisterTo:       os.ExpandEnv(cfg.RegisterTo),
		RegisterName:     os.ExpandEnv(cfg.RegisterName),
		RegisterEndpoint: os.ExpandEnv(cfg.RegisterEndpoint),
	}
}
