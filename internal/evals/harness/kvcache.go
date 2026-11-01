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
	"time"

	"github.com/go-steer/core-models/kvmetrics"

	"github.com/go-steer/core-agent/v3/internal/kvwindow"
)

// kvPollInterval spaces the readings inside one incident's run, so the
// peak KV utilization has more than the two endpoints to come from.
const kvPollInterval = 5 * time.Second

// kvSampling brackets each incident's run with the model server's
// KV-cache sampler, when the model under test publishes metrics. The
// first failed opening scrape turns it off for the rest of the run, so a
// dead endpoint costs one scrape timeout, not one per incident.
type kvSampling struct {
	s    *kvmetrics.Sampler
	note func(string, ...any)
}

func (k *kvSampling) start(ctx context.Context) *kvmetrics.Run {
	if k == nil || k.s == nil {
		return nil
	}
	run, err := k.s.Start(ctx, kvPollInterval)
	if err != nil {
		k.note("[kv] metrics scrape failed; KV-cache sampling is off for the rest of the run: %v", err)
		k.s = nil
		return nil
	}
	return run
}

// stop closes an incident's window. A window that lost its closing
// reading is kept, with EndFailed set; one with no usable readings is
// dropped.
func (k *kvSampling) stop(ctx context.Context, run *kvmetrics.Run) *kvwindow.Window {
	if run == nil {
		return nil
	}
	w, err := run.Stop(ctx)
	if err != nil && w.Readings == 0 {
		return nil
	}
	return kvwindow.From(w)
}

// kvCell renders an incident's window for the board: the counter hit
// rate, else the server's gauge, flagged when the window was cut short.
func kvCell(w *kvwindow.Window) string {
	if w == nil {
		return "-"
	}
	var cell string
	switch {
	case w.HitRate != nil:
		cell = fmt.Sprintf("%.0f%%", *w.HitRate*100)
	case w.HitRateGauge != nil:
		cell = fmt.Sprintf("gauge %.0f%%", *w.HitRateGauge*100)
	default:
		cell = "-"
	}
	if w.PeakKVUsage != nil {
		cell += fmt.Sprintf(" · peak KV %.1f%%", *w.PeakKVUsage*100)
	}
	if w.EndFailed {
		cell += " (closing scrape failed)"
	}
	return cell
}

// writeKVCache prints the corpus KV-cache line and each incident's. It
// prints nothing when the model under test publishes no metrics.
func (j *JudgeSummary) writeKVCache(p func(string, ...any)) {
	k := j.KVCache
	if k == nil || k.Windows == 0 {
		return
	}
	p("")
	p("model server KV cache (fleet-level: the server's whole traffic while each incident ran) — %d incident(s) sampled", k.Windows)
	line := "  corpus:"
	if k.HitRate != nil {
		line += fmt.Sprintf(" prefix hit rate %.1f%%", *k.HitRate*100)
	}
	if k.Hits != nil && k.Queries != nil {
		line += fmt.Sprintf(" (%.0f of %.0f tokens)", *k.Hits, *k.Queries)
	}
	if k.PeakKVUsage != nil {
		line += fmt.Sprintf(", peak KV %.1f%%", *k.PeakKVUsage*100)
	}
	if k.Reset {
		line += ", server restarted mid-run"
	}
	if k.EndFailed {
		line += ", some windows cut short"
	}
	p("%s", line)
	for _, s := range j.Scenes {
		if s.KVCache != nil {
			p("  %-42s %s", s.ID, kvCell(s.KVCache))
		}
	}
}
