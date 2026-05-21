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

	"github.com/go-steer/core-agent/config"
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

func TestMergeAttachOpts_ConfigSuppliesDefaults(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	opts := registerAttachFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}

	cfg := config.AttachConfig{
		Listen:           "0.0.0.0:7777",
		TLSCert:          "/etc/attach/tls.crt",
		TLSKey:           "/etc/attach/tls.key",
		ClientCA:         "/etc/attach/ca.crt",
		TokenEnv:         "ATTACH_TOKEN",
		ReadOnly:         true,
		PeerHub:          true,
		RegisterTo:       "https://hub.svc:7777",
		RegisterEndpoint: "https://10.0.0.7:7777",
		RegisterName:     "monitor-pod-1",
	}

	got := mergeAttachOpts(*opts, cfg, fs)
	want := attachOpts{
		Listen:           "0.0.0.0:7777",
		TLSCert:          "/etc/attach/tls.crt",
		TLSKey:           "/etc/attach/tls.key",
		ClientCA:         "/etc/attach/ca.crt",
		TokenEnv:         "ATTACH_TOKEN",
		ReadOnly:         true,
		PeerHub:          true,
		RegisterTo:       "https://hub.svc:7777",
		RegisterEndpoint: "https://10.0.0.7:7777",
		RegisterName:     "monitor-pod-1",
	}
	if got != want {
		t.Errorf("merged opts:\n got:  %+v\n want: %+v", got, want)
	}
}

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

func TestMergeAttachOpts_EnvExpansion(t *testing.T) {
	t.Setenv("POD_IP", "10.0.4.7")
	t.Setenv("MY_PORT", "7777")
	t.Setenv("MY_HOSTNAME", "pod-abc")

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	opts := registerAttachFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}

	cfg := config.AttachConfig{
		Listen:           "0.0.0.0:${MY_PORT}",
		RegisterEndpoint: "https://${POD_IP}:${MY_PORT}",
		RegisterName:     "monitor-${MY_HOSTNAME}",
	}
	got := mergeAttachOpts(*opts, cfg, fs)

	if got.Listen != "0.0.0.0:7777" {
		t.Errorf("Listen env-expansion: got %q", got.Listen)
	}
	if got.RegisterEndpoint != "https://10.0.4.7:7777" {
		t.Errorf("RegisterEndpoint env-expansion: got %q", got.RegisterEndpoint)
	}
	if got.RegisterName != "monitor-pod-abc" {
		t.Errorf("RegisterName env-expansion: got %q", got.RegisterName)
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
