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
// the URL by default, or a local file via --source), selects the
// Gemini and Anthropic models that can actually drive an agent loop,
// and emits a fresh builtin.go carrying their rates, their context
// windows, and a generation-time UpdatedAt on every entry.
//
// Selection is a RULE, not a list — see eligible() below. There used
// to be a hand-curated `allowlist` here, and it had no discovery path:
// the generator warned about listed-but-missing models, so a model
// nobody had thought to add was invisible forever. That is how six
// models reachable from the /model picker (both Gemini 2.5 entries,
// the two 3.1 pro variants, gemini-3-flash-preview and
// gemini-3.1-flash-image-preview) ended up with no builtin rate at
// all — which silently disables the --max-*-cost-usd ceilings, since
// an unpriced model contributes $0 to every budget check.
//
// Motivation: issue #259 showed that hand-authored builtin rates
// drift silently — the demo's gemini-3.5-flash entry was 20× too low
// on input, 30× too low on output. Regenerating from LiteLLM removes
// that class of drift; the UpdatedAt field lets operators see how
// old the current builtin snapshot is.
//
// Context windows ride along in the same file for the same reason.
// pkg/usage's hand-maintained table said gemini-2.5-pro held
// 2,000,000 input tokens (a carry-over from Gemini 1.5 Pro) when the
// real cap is 1,048,576, so mid-tier compaction was scheduled to fire
// at ~1.3M — past a hard limit the session would have died on first.
// LiteLLM publishes max_input_tokens for every model we select, so
// the number is now generated rather than remembered.
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
// Ownership: regenerate before every release, and REVIEW THE DIFF.
// Because selection is a rule, a regen can add or remove models on
// its own — not just move rates. `added` / `removed` lines in the
// --check report are the ones to read carefully: an addition means
// LiteLLM started publishing a model that satisfies eligible(), and a
// removal means one was deprecated upstream or lost its tool-calling
// flag. Both are usually correct and occasionally are upstream
// mistakes.
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
	"slices"
	"sort"
	"strings"
	"time"
)

// defaultLiteLLMSource is the upstream JSON URL. Pinned to the main
// branch so the generator always sees LiteLLM's current view; the
// generator's job is precisely to be a point-in-time snapshot.
const defaultLiteLLMSource = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// familyPrefixes limits selection to the two providers core-agent has
// adapters for. LiteLLM carries ~3000 entries across every router and
// reseller on the market; pricing a model we cannot resolve buys
// nothing and makes the diff unreadable.
var familyPrefixes = []string{"gemini-", "claude-"}

// nameExclusions drops models that pass every metadata check but are
// not general-purpose chat agents. Each needs a name rule because
// LiteLLM's own fields do not distinguish them: `mode` is "chat",
// `supports_function_calling` is true, and the modality arrays look
// exactly like a real chat model's. Keep the reason string — it is
// what a future reader needs to decide whether the rule still earns
// its place.
var nameExclusions = []struct{ Pattern, Why string }{
	{"-latest", "floating alias — identity AND price move underneath a pinned config, " +
		"and pkg/models/gemini's geminiMajorVersion() reads 0 from it, which makes " +
		"builtinsCompatible drop search grounding on every turn"},
	{"exp-", "unversioned experimental build; no stability promise from the provider"},
	{"computer-use", "computer-use model — a different tool surface, not an agent-loop chat model"},
	{"robotics", "embodied-reasoning model; not a chat agent"},
	{":", "provider-versioned id (Bedrock-style `…-v1:0`) that leaked into LiteLLM's " +
		"unprefixed namespace; the bare id is already selected"},
}

// liteLLMEntry mirrors the subset of LiteLLM's schema the generator
// consumes. LiteLLM's full entry has ~30 fields; we take the cost
// scalars, the context window, and the handful of capability signals
// eligible() screens on.
type liteLLMEntry struct {
	InputCostPerToken           *float64 `json:"input_cost_per_token,omitempty"`
	OutputCostPerToken          *float64 `json:"output_cost_per_token,omitempty"`
	CacheReadInputTokenCost     *float64 `json:"cache_read_input_token_cost,omitempty"`
	CacheCreationInputTokenCost *float64 `json:"cache_creation_input_token_cost,omitempty"`
	MaxInputTokens              *int     `json:"max_input_tokens,omitempty"`
	Mode                        string   `json:"mode,omitempty"`
	LiteLLMProvider             string   `json:"litellm_provider,omitempty"`

	// SupportsFunctionCalling is a POINTER on purpose. It is absent on
	// roughly 40% of LiteLLM's catalog, and absent must mean "unknown",
	// not "false" — decoding into a plain bool would silently drop
	// every model whose entry predates the flag.
	SupportsFunctionCalling *bool `json:"supports_function_calling,omitempty"`

	// DeprecationDate is an ISO date. Set on 15 entries today, every
	// one of them already in the past.
	DeprecationDate string `json:"deprecation_date,omitempty"`

	// SupportedOutputModalities catches the text-to-speech models that
	// declare mode "chat" (gemini-2.5-pro-preview-tts emits ["audio"]).
	// Nil on every Anthropic entry, so it can only ever exclude.
	SupportedOutputModalities []string `json:"supported_output_modalities,omitempty"`

	// SupportedEndpoints catches the Live/realtime variants
	// (gemini-3.1-flash-live-preview is ["/v1/realtime"]), which speak
	// a bidi protocol our transport does not. Also nil on Anthropic.
	SupportedEndpoints []string `json:"supported_endpoints,omitempty"`
}

// generatedEntry is what we render into the output file's map literals.
// Ordering is stable across regens (alphabetical on name) so diffs
// stay reviewable.
type generatedEntry struct {
	Name                      string
	InputPerMTok              float64
	CachedInputPerMTok        float64
	CacheCreationInputPerMTok float64
	OutputPerMTok             float64
	ContextWindowTokens       int
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
		"write nothing; print drift=true/drift=false on stdout depending on whether "+
			"--out matches LiteLLM's current catalog. UpdatedAt stamps are ignored, "+
			"so a merely-old file is not drift")
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
	// UpdatedAt for the whole batch, and the "today" that
	// deprecation_date is measured against. LiteLLM doesn't publish
	// per-entry timestamps, so every entry in one regen shares the same
	// verified-at date. Truncate to date-only (drop wall-clock time)
	// so identical regens on the same day produce identical output —
	// keeps diffs meaningful, and keeps --check stable within a day.
	now := time.Now().UTC().Truncate(24 * time.Hour)

	kept, rejected := selectModels(all, now)
	if len(kept) == 0 {
		die("no models satisfied eligible() — LiteLLM's schema has probably shifted; " +
			"inspect --source and the field names on liteLLMEntry before assuming the catalog emptied")
	}
	// Rejections are the discovery channel the old allowlist lacked: a
	// model that ALMOST qualifies (new family prefix, a flag upstream
	// hasn't set yet) shows up here instead of vanishing. Only the
	// near-misses are worth printing — the ~3000 entries filtered out
	// by family or mode would bury them.
	if len(rejected) > 0 {
		fmt.Fprintf(os.Stderr,
			"regen-builtin-pricing: %d Gemini/Anthropic entries rejected:\n", len(rejected))
		for _, r := range rejected {
			fmt.Fprintf(os.Stderr, "  %-44s %s\n", r.Name, r.Why)
		}
	}

	src, err := render(kept, now, *source)
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
	// One rendered rate row: `\t"model": {…},` plus its optional
	// // provider comment. Capture group 2 is everything after the name.
	reEntryLine = regexp.MustCompile(`(?m)^\t"([^"]+)": (\{[^}]*\},.*)$`)
	// One rendered window row: `\t"model": 1048576,`. Matched
	// separately from the rate row so a window-only change still gets
	// itemized instead of falling through to the "change is outside the
	// rate table" catch-all.
	reWindowLine = regexp.MustCompile(`(?m)^\t"([^"]+)": (\d+),$`)
)

// windowKeySuffix namespaces window rows inside entryMap so a model
// present in both generated maps doesn't have one row overwrite the
// other. It reads as part of the label in the --check report:
//
//	changed gemini-2.5-pro (context window):
const windowKeySuffix = " (context window)"

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

// entryMap indexes a normalized rendering by model name, covering both
// generated maps. Window rows are suffixed so they occupy their own key.
func entryMap(src []byte) map[string]string {
	out := map[string]string{}
	for _, m := range reEntryLine.FindAllSubmatch(src, -1) {
		out[string(m[1])] = string(m[2])
	}
	for _, m := range reWindowLine.FindAllSubmatch(src, -1) {
		out[string(m[1])+windowKeySuffix] = string(m[2])
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

// rejection records a Gemini/Anthropic entry that eligible() turned
// down, and why. Printed to stderr on every regen so the reason a
// model is absent is discoverable without re-deriving the predicate.
type rejection struct{ Name, Why string }

// eligible reports whether LiteLLM entry `name` should be baked into
// builtin.go, and if not, why. The bar is "core-agent could resolve
// this id and drive an agent loop with it".
//
// `family` reports whether the name was in scope at all — entries that
// fail it are not near-misses worth reporting, they are the other 2900
// models in the catalog.
func eligible(name string, e liteLLMEntry, today time.Time) (ok bool, family bool, why string) {
	// Provider-prefixed keys ("vertex_ai/gemini-3.5-flash",
	// "openrouter/google/…") name the same models through routers we
	// don't speak. core-agent's model ids are unprefixed.
	if strings.Contains(name, "/") {
		return false, false, ""
	}
	inFamily := false
	for _, p := range familyPrefixes {
		if strings.HasPrefix(name, p) {
			inFamily = true
			break
		}
	}
	if !inFamily {
		return false, false, ""
	}

	if e.Mode != "chat" {
		return false, true, fmt.Sprintf("mode is %q, not \"chat\"", e.Mode)
	}
	// Absent means unknown, not false — see the field comment.
	if e.SupportsFunctionCalling == nil {
		return false, true, "supports_function_calling not published; cannot confirm it can run tools"
	}
	if !*e.SupportsFunctionCalling {
		return false, true, "supports_function_calling is false; cannot drive an agent loop"
	}
	if e.InputCostPerToken == nil || e.OutputCostPerToken == nil {
		return false, true, "missing input/output cost fields"
	}
	if *e.InputCostPerToken == 0 && *e.OutputCostPerToken == 0 {
		return false, true, "zero cost (LiteLLM's not-published placeholder)"
	}
	if e.MaxInputTokens == nil || *e.MaxInputTokens <= 0 {
		return false, true, "no max_input_tokens; compaction would have no window to threshold against"
	}
	if e.DeprecationDate != "" {
		// Unparseable dates are treated as "not deprecated" rather than
		// dropping a live model over an upstream formatting change.
		if d, err := time.Parse("2006-01-02", e.DeprecationDate); err == nil && !d.After(today) {
			return false, true, "deprecated upstream on " + e.DeprecationDate
		}
	}
	if e.SupportedOutputModalities != nil && !slices.Contains(e.SupportedOutputModalities, "text") {
		return false, true, fmt.Sprintf("emits %v, not text", e.SupportedOutputModalities)
	}
	if e.SupportedEndpoints != nil && !slices.Contains(e.SupportedEndpoints, "/v1/chat/completions") {
		return false, true, fmt.Sprintf("endpoints %v exclude /v1/chat/completions", e.SupportedEndpoints)
	}
	for _, x := range nameExclusions {
		if strings.Contains(name, x.Pattern) {
			return false, true, fmt.Sprintf("name contains %q: %s", x.Pattern, x.Why)
		}
	}
	return true, true, ""
}

// selectModels applies eligible() across the whole catalog, returning
// the entries to render plus the in-family near-misses and why each
// was turned down. Both slices come back sorted by name so regens are
// byte-identical given the same input.
func selectModels(all map[string]liteLLMEntry, today time.Time) ([]generatedEntry, []rejection) {
	var kept []generatedEntry
	var rejected []rejection
	const million = 1_000_000.0
	for name, e := range all {
		ok, family, why := eligible(name, e, today)
		if !ok {
			if family {
				rejected = append(rejected, rejection{Name: name, Why: why})
			}
			continue
		}
		out := generatedEntry{
			Name:                name,
			ContextWindowTokens: *e.MaxInputTokens,
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
	sort.Slice(rejected, func(i, j int) bool { return rejected[i].Name < rejected[j].Name })
	return kept, rejected
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
	sb.WriteString(contextWindowDoc)
	sb.WriteString("var builtinContextWindows = map[string]int{\n")
	for _, e := range kept {
		fmt.Fprintf(&sb, "\t%q: %d,\n", e.Name, e.ContextWindowTokens)
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
//
// Membership is a rule, not a list: every Gemini and Anthropic model
// in LiteLLM's catalog that is mode="chat", supports function calling,
// carries a published price and window, and is not deprecated. See
// eligible() in dev/regen-builtin-pricing/main.go — adjust the rule
// there, never this file.
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

// contextWindowDoc documents the generated window table in the output
// file. LiteLLM's max_input_tokens is the authority here: the
// hand-maintained substring table in pkg/usage had gemini-2.5-pro at
// 2,000,000 (Gemini 1.5 Pro's number) against a real 1,048,576 cap.
const contextWindowDoc = `// builtinContextWindows is the max INPUT window per model, in tokens,
// straight from LiteLLM's max_input_tokens. Consumed by
// usage.ContextWindowSizeFor, which prefers an exact hit here over its
// substring fallback — the fallback still has to answer for operator
// ids that never appear in LiteLLM (long-context "-1m" suffixes,
// Vertex publication names), but for anything we ship a rate for, the
// number is generated rather than remembered.
//
// Note these are INPUT windows, not total: a model with a 1,048,576
// input cap may accept fewer once its max_output_tokens reservation is
// subtracted. Compaction thresholds are fractions of this number, so
// erring toward the input cap keeps the trigger conservative.
`

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

// BuiltinContextWindow returns the compiled-in max input window for
// modelID and whether one is known. Keys are lowercase, matching the
// rest of this package's case-insensitive lookup contract; callers
// holding an operator-typed id should lower it first.
//
// Separate from Rates because a context window is a capability, not a
// price: operator pricing overrides (.agents/pricing.json, ` + "`/pricing set`" + `)
// must not be able to move a model's window, and a model can be
// repriced without its window changing.
func BuiltinContextWindow(modelID string) (int, bool) {
	n, ok := builtinContextWindows[modelID]
	return n, ok
}

// BuiltinContextWindows returns a defensive copy of the whole window
// table, for tests and cross-table invariant checks.
func BuiltinContextWindows() map[string]int {
	out := make(map[string]int, len(builtinContextWindows))
	for k, v := range builtinContextWindows {
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
