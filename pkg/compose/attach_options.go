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
	"log"
	"os"
	"regexp"
	"strings"

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
		Listen:           expandEnvOrKeep(cfg.Listen),
		UnixSocket:       expandEnvOrKeep(cfg.UnixSocket),
		TLSCert:          expandEnvOrKeep(cfg.TLSCert),
		TLSKey:           expandEnvOrKeep(cfg.TLSKey),
		ClientCA:         expandEnvOrKeep(cfg.ClientCA),
		TokenEnv:         expandEnvOrKeep(cfg.TokenEnv),
		ReadOnly:         cfg.ReadOnly,
		PeerHub:          cfg.PeerHub,
		RegisterTo:       expandEnvOrKeep(cfg.RegisterTo),
		RegisterName:     expandEnvOrKeep(cfg.RegisterName),
		RegisterEndpoint: expandEnvOrKeep(cfg.RegisterEndpoint),
	}
}

// envRefPattern matches the two reference forms os.ExpandEnv
// understands for well-formed names: `$NAME` and `${NAME}`. Shell
// specials os.ExpandEnv would eat ($$, $1, a trailing lone $) are
// deliberately NOT matched — they pass through untouched, which is
// strictly more predictable than the old silent stripping.
var envRefPattern = regexp.MustCompile(`\$(?:\{[A-Za-z_][A-Za-z0-9_]*\}|[A-Za-z_][A-Za-z0-9_]*)`)

// expandEnvOrKeep is os.ExpandEnv that keeps an UNSET variable's
// reference literal — byte-for-byte, braces included — and logs a
// warning, instead of silently expanding it to "" (#488). The empty
// expansion turned typos into working-but-wrong config:
// `"127.0.0.1:$PORT"` with PORT unset became `"127.0.0.1:"`, a valid
// listen address on an ephemeral port. Keeping the reference verbatim
// makes the downstream error (bind failure, missing file) name the
// unresolved variable; preserving the exact spelling matters so
// `"pre-${X}suffix"` doesn't collapse into the different reference
// `$Xsuffix` (adversarial-review catch). Variables set to an empty
// string are honored as empty — "set but empty" is a deliberate
// operator choice; only "not set at all" is suspect.
func expandEnvOrKeep(s string) string {
	return envRefPattern.ReplaceAllStringFunc(s, func(ref string) string {
		name := strings.TrimFunc(ref[1:], func(r rune) bool { return r == '{' || r == '}' })
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		log.Printf("compose: attach config references $%s, which is not set; keeping it literal", name)
		return ref
	})
}
