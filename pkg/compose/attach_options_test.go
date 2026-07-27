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
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

func TestBuildAttachOptions_TranslatesEveryConfigField(t *testing.T) {
	t.Parallel()
	cfg := config.AttachConfig{
		Listen:           "0.0.0.0:7777",
		UnixSocket:       "/var/run/core-agent.sock",
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
	got := BuildAttachOptions(cfg)
	want := AttachOptions{
		Listen:           "0.0.0.0:7777",
		UnixSocket:       "/var/run/core-agent.sock",
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
		t.Errorf("BuildAttachOptions:\n got:  %+v\n want: %+v", got, want)
	}
}

func TestBuildAttachOptions_ExpandsEnvReferences(t *testing.T) {
	t.Setenv("POD_IP", "10.0.4.7")
	t.Setenv("MY_PORT", "7777")
	t.Setenv("MY_HOSTNAME", "pod-abc")

	got := BuildAttachOptions(config.AttachConfig{
		Listen:           "0.0.0.0:${MY_PORT}",
		RegisterEndpoint: "https://${POD_IP}:${MY_PORT}",
		RegisterName:     "monitor-${MY_HOSTNAME}",
	})
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

func TestBuildAttachOptions_ZeroConfigZeroOptions(t *testing.T) {
	t.Parallel()
	if got := BuildAttachOptions(config.AttachConfig{}); (got != AttachOptions{}) {
		t.Errorf("zero in, zero out — got: %+v", got)
	}
}
