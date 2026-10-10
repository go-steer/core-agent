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

package usage

import (
	"testing"

	"github.com/go-steer/core-models/profile"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// The no-catalog path (tests, library consumers) prices a profile's
// declared model the same way an installed catalog does, instead of
// reporting it unpriced.
func TestPriceForReadsProfileRatesWithoutACatalog(t *testing.T) {
	prev := globalCatalog.Load()
	globalCatalog.Store(nil)
	t.Cleanup(func() { globalCatalog.Store(prev) })

	cfg := config.DefaultConfig()
	cfg.Providers = []profile.Profile{{
		Name:    "house",
		Extends: "vllm",
		BaseURL: "http://10.0.0.2:8000/v1",
		Models:  []profile.Model{{ID: "house/model-a", Rates: &profile.Rates{InputPerMTok: 0.2, OutputPerMTok: 0.8}}},
	}}
	p := PriceFor("house/model-a", cfg)
	if p.Unpriced || p.InputPerMTok != 0.2 || p.OutputPerMTok != 0.8 {
		t.Errorf("PriceFor = %+v, want the declared 0.2/0.8", p)
	}
	if p := PriceFor("house/model-b", cfg); !p.Unpriced {
		t.Errorf("an undeclared model priced: %+v", p)
	}
}
