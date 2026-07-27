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
	"flag"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// registerAttachFlags wires the --attach-* flags onto fs, mirroring
// what main() does on flag.CommandLine. The flag set's parsed state is
// what mergeAttachOpts consults to detect explicit overrides.
func registerAttachFlags(fs *flag.FlagSet) *attachOpts {
	var o attachOpts
	fs.StringVar(&o.Listen, "attach-listen", "", "")
	fs.StringVar(&o.UnixSocket, "attach-unix-socket", "", "")
	fs.StringVar(&o.TLSCert, "attach-tls-cert", "", "")
	fs.StringVar(&o.TLSKey, "attach-tls-key", "", "")
	fs.StringVar(&o.ClientCA, "attach-client-ca", "", "")
	fs.StringVar(&o.TokenEnv, "attach-token", "", "")
	fs.BoolVar(&o.ReadOnly, "attach-readonly", false, "")
	fs.BoolVar(&o.PeerHub, "attach-peer-hub", false, "")
	fs.StringVar(&o.RegisterTo, "attach-register-to", "", "")
	fs.StringVar(&o.RegisterEndpoint, "attach-register-endpoint", "", "")
	fs.StringVar(&o.RegisterName, "attach-register-name", "", "")
	return &o
}

// Config-value translation (field mapping + ${ENV} expansion of
// config values) is covered in pkg/compose's BuildAttachOptions tests
// since #386 PR 6; what stays here is the CLI-beats-config precedence
// that needs flag.Visit.

func TestMergeAttachOpts_CLIBeatsConfig(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	opts := registerAttachFlags(fs)
	if err := fs.Parse([]string{
		"-attach-listen=:8888",
		"-attach-readonly=false", // explicit false must override config's true
		"-attach-register-name=cli-name",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	cfg := config.AttachConfig{
		Listen:       "0.0.0.0:7777",
		ReadOnly:     true,
		PeerHub:      true, // not overridden on CLI -> should still take effect
		RegisterName: "config-name",
	}

	got := mergeAttachOpts(*opts, cfg, fs)
	if got.Listen != ":8888" {
		t.Errorf("Listen: CLI should win, got %q", got.Listen)
	}
	if got.ReadOnly {
		t.Errorf("ReadOnly: explicit CLI false should beat config true")
	}
	if !got.PeerHub {
		t.Errorf("PeerHub: config true should stand when CLI did not set the flag")
	}
	if got.RegisterName != "cli-name" {
		t.Errorf("RegisterName: CLI should win, got %q", got.RegisterName)
	}
}

func TestMergeAttachOpts_EnvExpansionOnCLIValue(t *testing.T) {
	t.Setenv("POD_IP", "192.168.1.42")

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	opts := registerAttachFlags(fs)
	// CLI value carries an env-var reference. Expansion must still apply
	// (operators commonly template the value in the K8s manifest).
	if err := fs.Parse([]string{"-attach-register-endpoint=https://${POD_IP}:7777"}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	got := mergeAttachOpts(*opts, config.AttachConfig{}, fs)
	if got.RegisterEndpoint != "https://192.168.1.42:7777" {
		t.Errorf("RegisterEndpoint env-expansion on CLI value: got %q", got.RegisterEndpoint)
	}
}

func TestMergeAttachOpts_EmptyConfigEmptyFlags(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	opts := registerAttachFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}

	got := mergeAttachOpts(*opts, config.AttachConfig{}, fs)
	if (got != attachOpts{}) {
		t.Errorf("empty in, empty out — got: %+v", got)
	}
}

func TestMergeAttachOpts_ConfigDefaultsFlowThroughMerge(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	opts := registerAttachFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// One representative field per kind proves the merge consumes
	// compose.BuildAttachOptions for the config layer; exhaustive
	// field translation is BuildAttachOptions's own test in compose.
	got := mergeAttachOpts(*opts, config.AttachConfig{
		Listen:   "0.0.0.0:7777",
		ReadOnly: true,
	}, fs)
	if got.Listen != "0.0.0.0:7777" || !got.ReadOnly {
		t.Errorf("config defaults did not flow through merge: %+v", got)
	}
}
