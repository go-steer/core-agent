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

package harness

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-steer/core-models/kvmetrics"
	"github.com/go-steer/core-models/profile"

	"github.com/go-steer/core-agent/v3/internal/kvwindow"
)

func kvw(q, h, peak float64) *kvwindow.Window {
	r := h / q
	return &kvwindow.Window{Fleet: true, Queries: &q, Hits: &h, HitRate: &r, PeakKVUsage: &peak, Readings: 2}
}

// TestKVCacheBoard: the corpus line folds every sampled incident (counts
// summed, rate recomputed, peak the highest), each incident gets a row
// — the gauge when the server has no counters, and a flag when its
// window was cut short — and a board with nothing sampled prints
// nothing.
func TestKVCacheBoard(t *testing.T) {
	gauge := 0.5
	cut := kvw(300, 0, 0.005)
	cut.EndFailed = true
	j := &JudgeSummary{Scenes: []JudgeScenario{
		{ID: "LC-01", KVCache: kvw(100, 40, 0.2)},
		{ID: "LC-06", KVCache: cut},
		{ID: "LC-08", KVCache: &kvwindow.Window{Fleet: true, HitRateGauge: &gauge, Readings: 2}},
		{ID: "LC-10"},
	}}
	var f kvwindow.Fold
	for _, s := range j.Scenes {
		f.Add(s.KVCache)
	}
	j.KVCache = f.Window()
	if k := j.KVCache; k.Windows != 3 || *k.Queries != 400 || *k.Hits != 40 || *k.HitRate != 0.1 || *k.PeakKVUsage != 0.2 || !k.EndFailed || k.HitRateGauge != nil {
		t.Errorf("corpus fold = %+v", k)
	}
	var out strings.Builder
	j.writeKVCache(func(f string, a ...any) { fmt.Fprintf(&out, f+"\n", a...) })
	for _, want := range []string{
		"fleet-level", "3 incident(s) sampled", "prefix hit rate 10.0%", "(40 of 400 tokens)", "peak KV 20.0%", "some windows cut short",
		"LC-01", "40% · peak KV 20.0%", "peak KV 0.5% (closing scrape failed)", "gauge 50%",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("board missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "LC-10") {
		t.Errorf("an unsampled incident got a row:\n%s", out.String())
	}
	var empty strings.Builder
	(&JudgeSummary{}).writeKVCache(func(f string, a ...any) { fmt.Fprintf(&empty, f+"\n", a...) })
	if empty.Len() != 0 {
		t.Errorf("a board with no sampler printed:\n%s", empty.String())
	}
}

// TestKVSamplingTurnsOffAfterAFailedScrape: a dead endpoint costs one
// scrape for the whole run, not one per incident.
func TestKVSamplingTurnsOffAfterAFailedScrape(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	s, err := kvmetrics.New(kvmetrics.Options{URL: srv.URL, Format: profile.MetricsVLLM})
	if err != nil {
		t.Fatal(err)
	}
	var notes []string
	k := &kvSampling{s: s, note: func(f string, a ...any) { notes = append(notes, fmt.Sprintf(f, a...)) }}
	for range 5 {
		if w := k.stop(context.Background(), k.start(context.Background())); w != nil {
			t.Fatalf("a dead endpoint produced a window: %+v", w)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("%d scrapes over five incidents, want 1", n)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "off for the rest of the run") {
		t.Errorf("notes = %q", notes)
	}
}
