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
	"math"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-models/profile"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/pricing"
)

// isolatePricing clears the installed catalog and backend for one test.
func isolatePricing(t *testing.T) {
	t.Helper()
	prevCat, prevBackend := globalCatalog.Load(), globalBackend.Load()
	globalCatalog.Store(nil)
	globalBackend.Store(nil)
	t.Cleanup(func() {
		globalCatalog.Store(prevCat)
		globalBackend.Store(prevBackend)
	})
}

func cfgWithProvider(name string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Model.Provider = name
	return cfg
}

func TestBackendFor(t *testing.T) {
	isolatePricing(t)
	house := cfgWithProvider("house")
	house.Providers = []profile.Profile{{Name: "house", Extends: "vllm", BaseURL: "http://10.0.0.2:8000/v1"}}
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"nil cfg, nothing installed", nil, ""},
		{"auto-detected provider, nothing installed", cfgWithProvider(""), ""},
		{"core provider", cfgWithProvider(config.ProviderAnthropicVertex), "anthropic-vertex"},
		{"built-in profile", cfgWithProvider("openai"), "openai"},
		{"declared profile", house, "house"},
	} {
		if got := BackendFor(tc.cfg); got != tc.want {
			t.Errorf("%s: BackendFor = %q, want %q", tc.name, got, tc.want)
		}
	}

	SetBackend("vertex")
	if got := BackendFor(nil); got != "vertex" {
		t.Errorf("nil cfg after SetBackend: BackendFor = %q, want vertex", got)
	}
	if got := BackendFor(cfgWithProvider("")); got != "vertex" {
		t.Errorf("auto-detected provider after SetBackend: BackendFor = %q, want vertex", got)
	}
	if got := BackendFor(cfgWithProvider(config.ProviderAnthropic)); got != "anthropic" {
		t.Errorf("an explicit provider must beat the installed backend: BackendFor = %q, want anthropic", got)
	}
}

// OpenAI's models have no bare rows in the shared table: they price only
// when the lookup names the backend, which a nil-cfg cost site does
// through SetBackend.
func TestPriceForReachesBackendQualifiedRows(t *testing.T) {
	isolatePricing(t)
	const model = "gpt-5-mini"
	if _, ok := pricing.BuiltinByBackend()["openai/"+model]; !ok {
		t.Fatalf("the built-in table has no openai/%s row; pick another OpenAI model", model)
	}
	if p := PriceFor(model, nil); !p.Unpriced {
		t.Fatalf("%s priced with no backend: %+v; the test no longer proves anything", model, p)
	}
	if p := PriceFor(model, cfgWithProvider("openai")); p.Unpriced || p.IsZero() {
		t.Errorf("PriceFor(%s, provider openai) = %+v, want priced", model, p)
	}
	SetBackend("openai")
	if p := PriceFor(model, nil); p.Unpriced || p.IsZero() {
		t.Errorf("PriceFor(%s, nil) with backend openai installed = %+v, want priced", model, p)
	}
}

// A declared rate is keyed by backend, so two profiles that price one
// model id differently each bill their own; a lookup that can't name
// the backend still finds the first profile's, as before backends.
func TestDeclaredRatesAreKeyedByBackend(t *testing.T) {
	isolatePricing(t)
	cfg := cfgWithProvider("house-b")
	cfg.Providers = []profile.Profile{
		{Name: "house-a", Extends: "vllm", BaseURL: "http://10.0.0.2:8000/v1",
			Models: []profile.Model{{ID: "shared-model", Rates: &profile.Rates{InputPerMTok: 0.2, OutputPerMTok: 0.8}}}},
		{Name: "house-b", Extends: "vllm", BaseURL: "http://10.0.0.3:8000/v1",
			Models: []profile.Model{{ID: "shared-model", Rates: &profile.Rates{InputPerMTok: 0.5, OutputPerMTok: 2}}}},
	}
	if p := PriceFor("shared-model", cfg); p.InputPerMTok != 0.5 || p.OutputPerMTok != 2 {
		t.Errorf("on house-b: %+v, want house-b's 0.5/2", p)
	}
	cfg.Model.Provider = "house-a"
	if p := PriceFor("shared-model", cfg); p.InputPerMTok != 0.2 || p.OutputPerMTok != 0.8 {
		t.Errorf("on house-a: %+v, want house-a's 0.2/0.8", p)
	}
	// A nil cfg reaches declared rates only through an installed
	// catalog, which is how cmd/core-agent runs.
	c, err := pricing.NewCatalog(pricing.Options{Declared: DeclaredRates(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	SetCatalog(c)
	if p := PriceFor("shared-model", nil); p.InputPerMTok != 0.2 || p.OutputPerMTok != 0.8 {
		t.Errorf("no backend nameable: %+v, want the first profile's 0.2/0.8 (the bare key)", p)
	}
	SetBackend("house-b")
	if p := PriceFor("shared-model", nil); p.InputPerMTok != 0.5 || p.OutputPerMTok != 2 {
		t.Errorf("nil cfg with backend house-b installed: %+v, want house-b's 0.5/2", p)
	}
}

// A prompt over the model's long-context threshold bills every bucket
// and the output at the long-context rates; at or under it, the base.
func TestCostUSDForTurnAppliesLongContext(t *testing.T) {
	isolatePricing(t)
	p := PriceFor("gemini-2.5-pro", cfgWithProvider(config.ProviderVertex))
	if p.LongContext == nil {
		t.Fatalf("gemini-2.5-pro carries no long-context rate: %+v", p)
	}
	lc := p.LongContext
	short := TurnUsage{InputTokens: 100_000, OutputTokens: 1_000}
	long := TurnUsage{InputTokens: 300_000, OutputTokens: 1_000}
	want := func(in, out int, inRate, outRate float64) float64 {
		return float64(in)/1e6*inRate + float64(out)/1e6*outRate
	}
	if got, w := p.CostUSDForTurn(short), want(100_000, 1_000, p.InputPerMTok, p.OutputPerMTok); math.Abs(got-w) > 1e-12 {
		t.Errorf("under the threshold: %v, want base-rate %v", got, w)
	}
	if got, w := p.CostUSDForTurn(long), want(300_000, 1_000, lc.InputPerMTok, lc.OutputPerMTok); math.Abs(got-w) > 1e-12 {
		t.Errorf("over the threshold: %v, want long-context %v", got, w)
	}
	if got, w := p.CostUSD(300_000, 1_000), want(300_000, 1_000, lc.InputPerMTok, lc.OutputPerMTok); math.Abs(got-w) > 1e-12 {
		t.Errorf("CostUSD over the threshold: %v, want long-context %v", got, w)
	}
}

// TestIntroductoryRatesHaveNotOutlivedTheirExpiry keeps core-agent's own
// build red when a promotional rate it bills from has lapsed, whatever
// core-models version is pinned. core-models runs the same check in its
// own CI, but that can't stop core-agent shipping a pin whose table is
// past an expiry.
//
// The list, the dates and the judgment (IntroductoryRate.Check) live in
// core-models. What core-agent adds is which backends it serves each
// model on, checked through PriceFor, the lookup the ledger bills from.
func TestIntroductoryRatesHaveNotOutlivedTheirExpiry(t *testing.T) {
	isolatePricing(t)
	now := time.Now().UTC()
	for _, ir := range pricing.IntroductoryRates() {
		backends := introductoryBackends(ir.Model)
		if len(backends) == 0 {
			t.Errorf("%s: no core-agent provider serves this model family; teach introductoryBackends about it "+
				"so a new entry in core-models can't be skipped silently", ir.Model)
			continue
		}
		for _, backend := range backends {
			t.Run(backend+"/"+ir.Model, func(t *testing.T) {
				p := PriceFor(ir.Model, cfgWithProvider(backend))
				if p.Unpriced {
					t.Skipf("%s has no price on %s; the entry is noise for this backend", ir.Model, backend)
				}
				v := ir.Check(p.rates(), now)
				switch {
				case v.Fail:
					t.Errorf("%s on %s: %s\nFixes to the table land in go-steer/core-models; bump core-agent's pin once it has the new rate.",
						ir.Model, backend, v.Message)
				case v.Message != "":
					t.Logf("%s on %s: %s", ir.Model, backend, v.Message)
				}
			})
		}
	}
}

// introductoryBackends is the core providers core-agent serves a model
// family on.
func introductoryBackends(model string) []string {
	switch {
	case strings.HasPrefix(model, "claude-"):
		return []string{config.ProviderAnthropic, config.ProviderAnthropicVertex}
	case strings.HasPrefix(model, "gemini-"):
		return []string{config.ProviderGemini, config.ProviderVertex}
	}
	return nil
}

// PriceOn names the backend outright, for a cost site that knows which
// provider served the call but has no cfg: a subagent on a provider
// other than its parent's. It beats the installed process backend, and
// an empty backend is PriceFor(model, nil).
func TestPriceOnNamesTheBackend(t *testing.T) {
	isolatePricing(t)
	SetBackend("gemini") // the parent's
	if p := PriceOn("openai", "gpt-5-mini"); p.Unpriced || p.IsZero() || p.Backend != "openai" {
		t.Errorf(`PriceOn("openai", gpt-5-mini) under a gemini process = %+v, want priced on openai`, p)
	}
	if p := PriceOn("", "gpt-5-mini"); !p.Unpriced || p.Backend != "gemini" {
		t.Errorf(`PriceOn("", gpt-5-mini) = %+v, want PriceFor(nil)'s answer: unpriced, on the process backend`, p)
	}
	// Claude Sonnet 4.5's long-context rate exists on Vertex AI only.
	if p := PriceOn("anthropic-vertex", "claude-sonnet-4-5"); p.LongContext == nil {
		t.Errorf("anthropic-vertex claude-sonnet-4-5: no long-context rate: %+v", p)
	}
	if p := PriceOn("anthropic", "claude-sonnet-4-5"); p.LongContext != nil {
		t.Errorf("anthropic claude-sonnet-4-5: long-context rate %+v, want none", p.LongContext)
	}
}

// The tracker keeps the backend each turn was priced on, so a later
// re-pricing (the /usage savings reference) asks the same backend.
func TestTrackerRecordsTheBackend(t *testing.T) {
	isolatePricing(t)
	tr := NewTracker()
	tr.AppendUsage("gpt-5-mini", TurnUsage{InputTokens: 1000, OutputTokens: 100}, PriceOn("openai", "gpt-5-mini"))
	tr.AppendSideUsage("gemini-3.6-flash", TurnUsage{InputTokens: 10}, PriceOn("", "gemini-3.6-flash"))
	turns := tr.All()
	if len(turns) != 2 || turns[0].Backend != "openai" || turns[1].Backend != "" {
		t.Fatalf("turn backends = %+v, want openai then \"\"", turns)
	}
	if turns[0].Unpriced || turns[0].CostUSD <= 0 {
		t.Errorf("openai turn: unpriced=%v cost=%v, want priced", turns[0].Unpriced, turns[0].CostUSD)
	}
}
