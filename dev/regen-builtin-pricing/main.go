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

// Generator for pkg/pricing/builtin.go.
//
// Reads BerriAI/litellm's model_prices_and_context_window.json (from
// the URL by default, or a local file via --source), filters to the
// curated allowlist below, and emits a fresh builtin.go with a
// generation-time UpdatedAt on every entry.
//
// Motivation: issue #259 showed that hand-authored builtin rates
// drift silently — the demo's gemini-3.5-flash entry was 20× too low
// on input, 30× too low on output. Regenerating from LiteLLM removes
// that class of drift; the UpdatedAt field lets operators see how
// old the current builtin snapshot is.
//
// Usage:
//
//	# Regenerate from LiteLLM's live master:
//	go run ./dev/regen-builtin-pricing
//
//	# From a pinned local snapshot (e.g. reviewing what would change
//	# without hitting the network):
//	go run ./dev/regen-builtin-pricing --source=/tmp/litellm.json
//
//	# Preview to stdout without writing:
//	go run ./dev/regen-builtin-pricing --stdout
//
//	# Ask "have any RATES moved?" without writing anything:
//	go run ./dev/regen-builtin-pricing --check
//
// --check reports its answer on STDOUT as a single line, either
// `drift=true` or `drift=false`, and exits 0 either way. Human-readable
// detail (which models moved, and how) goes to stderr. A non-zero exit
// from --check always means the generator itself failed — bad JSON,
// unreachable network, unreadable --out.
//
// Drift is deliberately NOT signalled through the exit code, because
// `go run` collapses every non-zero child status to 1: a program that
// exits 2 makes `go run` print "exit status 2" and then exit 1 itself.
// All four callers invoke this through `go run`, so an exit-code
// convention would make a failed fetch indistinguishable from a real
// price change — and the weekly workflow would open a pull request
// every time GitHub's egress hiccuped.
//
// Ownership: regenerate before every release. Review the diff — the
// allowlist is stable but LiteLLM occasionally shifts rates. Commit
// both the regenerated builtin.go and any allowlist changes together.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// defaultLiteLLMSource is the upstream JSON URL. Pinned to the main
// branch so the generator always sees LiteLLM's current view; the
// generator's job is precisely to be a point-in-time snapshot.
const defaultLiteLLMSource = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// allowlist names the models to include in the generated builtin.
// Curated for the demo + common alternatives operators reach for.
// Entries missing from LiteLLM at regen time are silently skipped
// (logged to stderr) — better than a compile break when a name gets
// renamed upstream.
//
// Grow this list conservatively: every entry becomes an "we vouch
// for this rate at generation time" claim baked into the binary.
// When a model gets deprecated by its provider, remove it here and
// regenerate.
var allowlist = []string{
	// Gemini 3.x — the demo's primary family. 3.6-flash and
	// 3.5-flash are the taskclass frontier/mid defaults (#530/#531)
	// so their absence here would leave the default models unpriced;
	// if Google adds a pro-line entry back to LiteLLM, add it here
	// (the regen tool logs skipped-but-listed models to stderr,
	// which is how we'd notice).
	"gemini-3.7-flash",
	"gemini-3.6-flash",
	"gemini-3.5-flash",
	"gemini-3.5-flash-lite",
	"gemini-3.1-flash-lite",

	// Anthropic Claude 4/5 — common alternative. Every taskclass
	// tier default MUST appear here: builtin is the air-gapped /
	// refresh-failed pricing floor, and an unpriced default leaves
	// --max-*-cost-usd ceilings silently inert exactly where
	// daemons run unattended. Pinned by pricing's
	// TestBuiltin_CoversTaskclassTierDefaults.
	"claude-opus-4-8",
	"claude-opus-5",
	"claude-fable-5",
	"claude-sonnet-5",
	"claude-sonnet-4-6",
	"claude-haiku-4-5",
	"claude-opus-4-7",
}

// liteLLMEntry mirrors the subset of LiteLLM's schema the generator
// consumes. LiteLLM's full entry has ~30 fields (context window,
// modality flags, endpoint hints); we only care about the cost
// scalars + a stability signal to filter garbage.
type liteLLMEntry struct {
	InputCostPerToken           *float64 `json:"input_cost_per_token,omitempty"`
	OutputCostPerToken          *float64 `json:"output_cost_per_token,omitempty"`
	CacheReadInputTokenCost     *float64 `json:"cache_read_input_token_cost,omitempty"`
	CacheCreationInputTokenCost *float64 `json:"cache_creation_input_token_cost,omitempty"`
	Mode                        string   `json:"mode,omitempty"`
	LiteLLMProvider             string   `json:"litellm_provider,omitempty"`
}

// generatedEntry is what we render into the output file's map literal.
// Ordering is stable across regens (alphabetical on name) so diffs
// stay reviewable.
type generatedEntry struct {
	Name                      string
	InputPerMTok              float64
	CachedInputPerMTok        float64
	CacheCreationInputPerMTok float64
	OutputPerMTok             float64
	Provider                  string
}

func main() {
	source := flag.String("source", defaultLiteLLMSource,
		"URL or path to LiteLLM's model_prices_and_context_window.json")
	outPath := flag.String("out", defaultOutPath(),
		"path to write generated builtin.go (default: pkg/pricing/builtin.go relative to cwd)")
	toStdout := flag.Bool("stdout", false,
		"print generated file to stdout instead of writing to --out")
	check := flag.Bool("check", false,
		"write nothing; exit 1 if --out's rates differ from LiteLLM's current "+
			"catalog. UpdatedAt stamps are ignored, so a merely-old file is not drift")
	flag.Parse()

	if *check && *toStdout {
		die("--check and --stdout are mutually exclusive")
	}

	body, err := load(*source)
	if err != nil {
		die("load %s: %v", *source, err)
	}
	all, err := parse(body)
	if err != nil {
		die("parse: %v", err)
	}
	kept, missing := filter(all, allowlist)
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "regen-builtin-pricing: %d allowlist entries not in LiteLLM (skipping): %s\n",
			len(missing), strings.Join(missing, ", "))
	}
	if len(kept) == 0 {
		die("no allowlist models matched — LiteLLM schema may have shifted or the allowlist needs updating")
	}

	// UpdatedAt for the whole batch. LiteLLM doesn't publish per-entry
	// timestamps, so every entry in one regen shares the same
	// verified-at date. Truncate to date-only (drop wall-clock time)
	// so identical regens on the same day produce identical output —
	// keeps diffs meaningful.
	now := time.Now().UTC().Truncate(24 * time.Hour)
	source_ := *source
	src, err := render(kept, now, source_)
	if err != nil {
		die("render: %v", err)
	}

	if *check {
		checkDrift(*outPath, src)
		return
	}
	if *toStdout {
		if _, err := os.Stdout.Write(src); err != nil {
			die("write stdout: %v", err)
		}
		return
	}
	if err := os.WriteFile(*outPath, src, 0o644); err != nil { //nolint:gosec // generator output, not user data
		die("write %s: %v", *outPath, err)
	}
	fmt.Fprintf(os.Stderr, "regen-builtin-pricing: wrote %d entries to %s (UpdatedAt=%s)\n",
		len(kept), *outPath, now.Format("2006-01-02"))
}

// --- drift checking (--check) --------------------------------------
//
// Every regen stamps time.Now() onto every entry, so `git diff` against
// the committed builtin.go reports a change on any day the file wasn't
// regenerated — even when LiteLLM hasn't moved a single rate. Four
// callers (pricing-regen.yml, release.yml, and the two cut-*-tag.sh
// scripts) used that diff as their drift signal, which meant they were
// really testing the calendar: the weekly workflow would have opened a
// no-op PR every Monday, and the release guards fire unless you regen
// and commit on the same UTC day you tag. --check compares the
// rate-bearing content with the timestamps normalized away.

var (
	// UpdatedAt literals: time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC).
	reUpdatedAt = regexp.MustCompile(`time\.Date\(\d{4}, \d{1,2}, \d{1,2}, 0, 0, 0, 0, time\.UTC\)`)
	// Header line: "// Regenerated 2026-08-16 from https://...". The
	// source is normalized away along with the date so that checking a
	// pinned local snapshot (--check --source=/tmp/litellm.json) doesn't
	// report drift purely because the provenance string differs from
	// the committed file's URL.
	reHeaderLine = regexp.MustCompile(`(?m)^// Regenerated \d{4}-\d{2}-\d{2} from .*$`)
	// gofmt column-aligns the map literal, so one entry gaining a digit
	// re-pads every row in the block. Without collapsing runs of spaces
	// the report blames a dozen models whose rates never moved.
	// Indentation is a tab, so rows keep their leading whitespace.
	reRunOfSpaces = regexp.MustCompile(`  +`)
	// One rendered row: `\t"model": {…},` plus its optional // provider
	// comment. Capture group 2 is everything after the model name.
	reEntryLine = regexp.MustCompile(`(?m)^\t"([^"]+)": (\{[^}]*\},.*)$`)
)

// normalize strips the two things that change on every regen regardless
// of upstream rates — the per-entry UpdatedAt and the header's
// "Regenerated" date — plus gofmt's alignment padding.
func normalize(src []byte) []byte {
	src = reUpdatedAt.ReplaceAll(src, []byte("time.Date(<UPDATED-AT>)"))
	src = reHeaderLine.ReplaceAll(src, []byte("// Regenerated <DATE> from <SOURCE>."))
	return reRunOfSpaces.ReplaceAll(src, []byte(" "))
}

// checkDrift compares freshly generated source against what is on disk
// at outPath. The verdict goes to stdout as `drift=true` / `drift=false`
// for callers to branch on; the itemized explanation goes to stderr for
// humans reading CI logs. Genuine failures exit non-zero via die, which
// is what callers must treat as "could not determine".
func checkDrift(outPath string, generated []byte) {
	existing, err := os.ReadFile(outPath) //nolint:gosec // caller-supplied path
	if err != nil {
		die("read %s for --check: %v", outPath, err)
	}
	want, got := normalize(generated), normalize(existing)
	if bytes.Equal(want, got) {
		fmt.Fprintf(os.Stderr,
			"regen-builtin-pricing: %s is up to date — rates match LiteLLM (UpdatedAt ignored)\n",
			outPath)
		fmt.Println("drift=false")
		return
	}
	fmt.Fprintf(os.Stderr,
		"regen-builtin-pricing: %s is stale — LiteLLM rates have moved.\n\n", outPath)
	lines := diffEntries(got, want)
	if len(lines) == 0 {
		// Non-empty diff with no per-entry change means the header or
		// the accessor block moved (e.g. the generator's own template
		// changed). Still drift; just not something we can itemize.
		lines = []string{"(change is outside the rate table — regenerate and review the diff)"}
	}
	for _, l := range lines {
		fmt.Fprintf(os.Stderr, "  %s\n", l)
	}
	fmt.Fprintf(os.Stderr, "\nRefresh with: go run ./dev/regen-builtin-pricing\n")
	fmt.Println("drift=true")
}

// diffEntries itemizes per-model differences between two normalized
// renderings, so CI logs name the models whose rates moved instead of
// dumping a whole-file diff.
func diffEntries(oldSrc, newSrc []byte) []string {
	oldE, newE := entryMap(oldSrc), entryMap(newSrc)
	names := make(map[string]struct{}, len(oldE)+len(newE))
	for n := range oldE {
		names[n] = struct{}{}
	}
	for n := range newE {
		names[n] = struct{}{}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	var out []string
	for _, n := range sorted {
		o, inOld := oldE[n]
		w, inNew := newE[n]
		switch {
		case inOld && !inNew:
			out = append(out, fmt.Sprintf("removed %s: %s", n, o))
		case !inOld && inNew:
			out = append(out, fmt.Sprintf("added   %s: %s", n, w))
		case o != w:
			out = append(out,
				fmt.Sprintf("changed %s:", n),
				fmt.Sprintf("    was %s", o),
				fmt.Sprintf("    now %s", w))
		}
	}
	return out
}

// entryMap indexes a normalized rendering by model name.
func entryMap(src []byte) map[string]string {
	out := map[string]string{}
	for _, m := range reEntryLine.FindAllSubmatch(src, -1) {
		out[string(m[1])] = string(m[2])
	}
	return out
}

// load reads the LiteLLM JSON from either a local path or an http(s)
// URL. Local paths are handy for offline review of "what would change
// if we regenerated now" without hitting the network.
func load(source string) ([]byte, error) {
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		req, err := http.NewRequest(http.MethodGet, source, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("http %d fetching %s", resp.StatusCode, source)
		}
		return io.ReadAll(resp.Body)
	}
	return os.ReadFile(source) //nolint:gosec // caller-supplied path
}

// parse decodes LiteLLM's JSON into a per-model map. Malformed entries
// are dropped rather than failing the whole run — LiteLLM's schema
// evolves and one weird entry shouldn't break regen.
func parse(body []byte) (map[string]liteLLMEntry, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("json unmarshal: %w", err)
	}
	out := make(map[string]liteLLMEntry, len(raw))
	for name, payload := range raw {
		var e liteLLMEntry
		if err := json.Unmarshal(payload, &e); err != nil {
			continue
		}
		out[name] = e
	}
	return out, nil
}

// filter picks the entries in allowlist that have usable cost data,
// returning the kept entries + the names that weren't found. Missing
// entries are reported so the operator regenerating knows their
// allowlist has drifted.
func filter(all map[string]liteLLMEntry, allow []string) ([]generatedEntry, []string) {
	var kept []generatedEntry
	var missing []string
	const million = 1_000_000.0
	for _, name := range allow {
		e, ok := all[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		if e.InputCostPerToken == nil || e.OutputCostPerToken == nil {
			missing = append(missing, name+" (missing cost fields)")
			continue
		}
		if *e.InputCostPerToken == 0 && *e.OutputCostPerToken == 0 {
			missing = append(missing, name+" (zero cost)")
			continue
		}
		out := generatedEntry{
			Name: name,
			// Round to 6 decimals so binary-repr artifacts from
			// per-token → per-Mtok multiplication (0.0000001 * 1M
			// producing 0.09999999999999999 instead of 0.1) don't
			// pollute the file. Six decimals = $0.000001/M, orders
			// of magnitude finer than any real rate we'll see.
			InputPerMTok:  round6(*e.InputCostPerToken * million),
			OutputPerMTok: round6(*e.OutputCostPerToken * million),
			Provider:      e.LiteLLMProvider,
		}
		if e.CacheReadInputTokenCost != nil && *e.CacheReadInputTokenCost > 0 {
			out.CachedInputPerMTok = round6(*e.CacheReadInputTokenCost * million)
		}
		// Cache-WRITE rate (Anthropic's cache_creation_input_tokens).
		// Absent for models without prompt-cache writes; zero is
		// LiteLLM's "not supported" placeholder, same as the read rate.
		if e.CacheCreationInputTokenCost != nil && *e.CacheCreationInputTokenCost > 0 {
			out.CacheCreationInputPerMTok = round6(*e.CacheCreationInputTokenCost * million)
		}
		kept = append(kept, out)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })
	return kept, missing
}

// render produces the final gofmt'd builtin.go source. Header
// documents when + from where + how to regenerate so the next
// contributor doesn't have to reconstruct that context.
func render(kept []generatedEntry, updatedAt time.Time, source string) ([]byte, error) {
	var sb strings.Builder
	sb.WriteString(fileHeader(updatedAt, source))
	sb.WriteString("var builtin = map[string]Rates{\n")
	for _, e := range kept {
		sb.WriteString(renderEntry(e, updatedAt))
	}
	sb.WriteString("}\n\n")
	sb.WriteString(builtinAccessor)
	return format.Source([]byte(sb.String()))
}

func renderEntry(e generatedEntry, updatedAt time.Time) string {
	// time.Date literal keeps the file self-contained (no time.Parse
	// at runtime, no init cost). Truncated to date so identical
	// same-day regens produce identical output.
	tsLit := fmt.Sprintf("time.Date(%d, %d, %d, 0, 0, 0, 0, time.UTC)",
		updatedAt.Year(), int(updatedAt.Month()), updatedAt.Day())
	prov := ""
	if e.Provider != "" {
		prov = fmt.Sprintf(" // %s", e.Provider)
	}
	// Optional rate fields are emitted only when present so rows for
	// models without prompt caching stay short and the diff between
	// regens stays readable.
	fields := []string{fmt.Sprintf("InputPerMTok: %v", e.InputPerMTok)}
	if e.CachedInputPerMTok > 0 {
		fields = append(fields, fmt.Sprintf("CachedInputPerMTok: %v", e.CachedInputPerMTok))
	}
	if e.CacheCreationInputPerMTok > 0 {
		fields = append(fields, fmt.Sprintf("CacheCreationInputPerMTok: %v", e.CacheCreationInputPerMTok))
	}
	fields = append(fields,
		fmt.Sprintf("OutputPerMTok: %v", e.OutputPerMTok),
		fmt.Sprintf("UpdatedAt: %s", tsLit))
	return fmt.Sprintf("\t%q: {%s},%s\n", e.Name, strings.Join(fields, ", "), prov)
}

func fileHeader(updatedAt time.Time, source string) string {
	return fmt.Sprintf(`// Copyright 2026 Google LLC
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

// Code generated by dev/regen-builtin-pricing. DO NOT EDIT.
//
// Regenerated %s from %s.
//
// To refresh: %sgo run ./dev/regen-builtin-pricing%s
// Curate the allowlist in dev/regen-builtin-pricing/main.go.
//
// Issue #259 context: this file used to be hand-authored, drifted
// silently, and shipped rates that were off by 20-30x during the
// v2.7.0-dev.3 demo drive. The regen path is the mitigation — every
// entry carries an UpdatedAt so operators can spot stale entries via
// /pricing, and the whole file rebuilds from LiteLLM's authoritative
// catalog rather than accreting hand-edits.

package pricing

import "time"

`, updatedAt.Format("2006-01-02"), source, "`", "`")
}

const builtinAccessor = `// Builtin returns a defensive copy of the compiled-in table. Used
// by tests + by tools that want to inspect what shipped (e.g. a
// future ` + "`/pricing list builtin`" + ` view).
func Builtin() map[string]Rates {
	out := make(map[string]Rates, len(builtin))
	for k, v := range builtin {
		out[k] = v
	}
	return out
}
`

// defaultOutPath resolves pkg/pricing/builtin.go relative to the
// current working directory. Assumes the generator is run from the
// repo root (the go run invocation from README's usage block).
func defaultOutPath() string {
	return filepath.Join("pkg", "pricing", "builtin.go")
}

// die reports a generator failure. Any non-zero exit means "the
// generator could not run" — never "rates drifted", which --check
// reports on stdout instead. See the --check note at the top of this
// file for why the exit code can't carry that distinction.
func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "regen-builtin-pricing: "+format+"\n", args...)
	os.Exit(1)
}

// round6 truncates x to 6 decimal places. Undoes binary-repr noise
// introduced when LiteLLM's per-token rates (like 0.0000001) are
// multiplied by 1M and end up as 0.09999999999999999 instead of 0.1.
// Six decimals = one-millionth-of-a-dollar per Mtok, way finer than
// any real rate we care about.
func round6(x float64) float64 {
	const scale = 1_000_000.0
	rounded := float64(int64(x*scale+0.5)) / scale
	// Preserve exact zero (avoid returning -0 from the rounding above).
	if rounded == 0 {
		return 0
	}
	return rounded
}
