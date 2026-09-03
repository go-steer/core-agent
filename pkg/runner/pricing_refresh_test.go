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

package runner

import (
	"context"
	"iter"
	"strings"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/internal/testutil"
	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/pricing"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// #930. The daemon resolved usage.PriceFor once at boot and handed the
// resulting VALUE to every billing path. `POST /pricing/refresh` and
// `/pricing/set` rebuild the catalog and install it with SetCatalog,
// which cannot reach a copy that has already been made — so /pricing
// reported the new rate and the ledger kept charging the old one.
//
// These tests install a process-global catalog and so are not parallel.

func turnEvent(in, out int32) *session.Event {
	return &session.Event{
		LLMResponse: adkmodel.LLMResponse{
			TurnComplete: true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     in,
				CandidatesTokenCount: out,
				TotalTokenCount:      in + out,
			},
		},
	}
}

func seqOf(evs ...*session.Event) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		for _, ev := range evs {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func installRate(t *testing.T, model string, in, out float64) {
	t.Helper()
	c, err := pricing.NewCatalog(pricing.Options{
		CfgOverride: map[string]pricing.ModelRates{
			model: {InputPerMTok: in, OutputPerMTok: out},
		},
	})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	usage.SetCatalog(c)
}

// The tap that bills every headless AND every REPL turn — replCore
// drives each turn through streamTurn, which builds this wrapper — so
// a stale rate here spanned the whole life of an interactive session,
// not one prompt.
//
// Pre-fix this bills 2 × the stale $1/MTok input rate and the assertion
// below reads $2.00 instead of $20.00.
func TestTapTracker_BillsTheRefreshedRateNotTheCapturedOne(t *testing.T) {
	usage.SetCatalog(nil)
	t.Cleanup(func() { usage.SetCatalog(nil) })

	// What the daemon captured at boot.
	captured := usage.Pricing{InputPerMTok: 1, OutputPerMTok: 1}
	// What the operator's /pricing refresh installed afterwards.
	installRate(t, "m", 10, 10)

	tr := usage.NewTracker()
	events := tapTracker(seqOf(turnEvent(1_000_000, 1_000_000)), tr, "m", captured)
	for range events { //nolint:revive // draining the iterator is the point
	}

	got := tr.Totals().CostUSD
	if !nearUSD(got, 20) {
		t.Errorf("billed $%.4f, want $20.0000 at the refreshed 10/10 rate (the captured 1/1 rate bills $2) — "+
			"/pricing reported the new rate while the ledger and the --max-session-cost-usd ceiling used the old one", got)
	}
}

// A caller with no installed catalog keeps the rate it passed in.
// Without this, the fix would silently reprice every embedder's turns
// against the builtin table.
func TestTapTracker_NoCatalogKeepsTheCallersRate(t *testing.T) {
	usage.SetCatalog(nil)
	t.Cleanup(func() { usage.SetCatalog(nil) })

	tr := usage.NewTracker()
	events := tapTracker(seqOf(turnEvent(1_000_000, 0)), tr, "m",
		usage.Pricing{InputPerMTok: 3, OutputPerMTok: 3})
	for range events { //nolint:revive // draining the iterator is the point
	}

	if got := tr.Totals().CostUSD; !nearUSD(got, 3) {
		t.Errorf("billed $%.4f, want $3.0000 from the caller's own rate", got)
	}
}

// Events must still pass through untouched — the tap is transparent by
// contract, and WriteEvents downstream is an opaque consumer.
func TestTapTracker_PassesEventsThrough(t *testing.T) {
	usage.SetCatalog(nil)
	t.Cleanup(func() { usage.SetCatalog(nil) })
	installRate(t, "m", 10, 10)

	in := []*session.Event{turnEvent(10, 10), turnEvent(20, 20)}
	var seen int
	for range tapTracker(seqOf(in...), usage.NewTracker(), "m", usage.Pricing{}) {
		seen++
	}
	if seen != len(in) {
		t.Errorf("saw %d events, want %d", seen, len(in))
	}
}

// The multi-session daemon's live billing path. WakeLoop's Pricing is
// resolved once when the session is CONSTRUCTED
// (pkg/compose/multi_session.go), and a wake loop then runs for the
// life of the session — so this is the longest stale window of the
// four, and the one an operator refreshing rates on a days-old daemon
// actually hits.
//
// Pre-fix this bills at the captured 1/1 rate and reads $1 instead of
// $10.
func TestWakeLoop_BillsTheRefreshedRateNotTheCapturedOne(t *testing.T) {
	usage.SetCatalog(nil)
	t.Cleanup(func() { usage.SetCatalog(nil) })
	installRate(t, "m", 10, 10)

	m := &testutil.FakeModel{
		ModelName: "m",
		Script:    []testutil.ScriptedResponse{{TextChunks: []string{"ok"}, InputTokens: 1_000_000}},
	}
	a, err := agent.New(m)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	tr := usage.NewTracker()
	done := make(chan struct{}, 4)
	turnErrs := make(chan error, 4)
	go WakeLoop(ctx, a, WakeLoopOptions{
		Tracker: tr,
		Model:   "m",
		// What the session captured when it was built.
		Pricing:     usage.Pricing{InputPerMTok: 1, OutputPerMTok: 1},
		OnTurnError: func(err error) { turnErrs <- err },
		Debugf: func(format string, _ ...any) {
			if strings.HasPrefix(format, "Run finished") {
				select {
				case done <- struct{}{}:
				default:
				}
			}
		},
	})

	if err := a.Inject("go"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	select {
	case err := <-turnErrs:
		t.Fatalf("turn error: %v", err)
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("WakeLoop never completed a turn after Inject")
	}

	if got := tr.Totals().CostUSD; !nearUSD(got, 10) {
		t.Errorf("billed $%.4f, want $10.0000 at the refreshed 10/MTok input rate (the rate captured at session construction bills $1)", got)
	}
}

func nearUSD(got, want float64) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
