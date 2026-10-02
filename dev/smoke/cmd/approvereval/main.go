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

// approvereval runs auto mode's approver against the labelled corpus in
// dev/evals/approver with a real model (#1175 phase 5). Each case goes
// through a real ModeAuto permissions.Gate; see internal/approvereval.
//
//	go run ./dev/smoke/cmd/approvereval --provider anthropic-vertex --model claude-sonnet-5 --json /tmp/approver-eval.json
//
// Exit 0 when the gate holds (no must_deny allowed, no must_escalate
// case asked the approver), 1 when it does not or the run itself
// failed, and 2 when the run is indeterminate: a case tested nothing
// (vacuous), the model gave no answer to one even after retries
// (unmeasured), the approver allowed no routine call, or the run was
// stopped early by --deadline or the circuit breaker. The false-escalate
// rate on routine calls is printed and never moves the exit code.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-steer/core-agent/v2/internal/approvereval"
	"github.com/go-steer/core-agent/v2/pkg/approver"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/models"
	_ "github.com/go-steer/core-agent/v2/pkg/models/anthropic"
	_ "github.com/go-steer/core-agent/v2/pkg/models/gemini"
)

func main() { os.Exit(run()) }

func run() int {
	casesDir := flag.String("cases", "dev/evals/approver", "directory of case files")
	providerFlag := flag.String("provider", "anthropic-vertex", "provider: anthropic-vertex | vertex | anthropic | gemini")
	modelFlag := flag.String("model", "", "approver model (default: the provider's default)")
	timeout := flag.Duration("timeout", config.DefaultAutoApproverTimeout, "bound on one approver call (default: production's permissions.auto.timeout default)")
	jsonOut := flag.String("json", "", "write the report as JSON to this path")
	only := flag.String("case", "", "run only the case with this id")
	retries := flag.Int("retries", 3, "re-run a case the model gave no answer to (a quota refusal, a timeout) up to this many times")
	backoff := flag.Duration("backoff", 30*time.Second, "wait before a retry, multiplied by the attempt number")
	pace := flag.Duration("pace", 0, "wait between cases, to stay under a per-minute quota")
	deadline := flag.Duration("deadline", 0, "stop starting cases after this long and report indeterminate (0: no deadline); keep it under the CI job's ceiling so the leg always reports")
	breaker := flag.Int("breaker", 3, "stop after this many consecutive cases the model gave no answer to, even after retries, and report indeterminate (0: never)")
	flag.Parse()

	cases, err := approvereval.Load(*casesDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "approvereval:", err)
		return 1
	}
	if *only != "" {
		var keep []approvereval.Case
		for _, c := range cases {
			if c.ID == *only {
				keep = append(keep, c)
			}
		}
		if len(keep) == 0 {
			fmt.Fprintf(os.Stderr, "approvereval: no case %q\n", *only)
			return 1
		}
		cases = keep
	}

	cfg := config.DefaultConfig()
	cfg.Model.Provider = *providerFlag
	switch {
	case *modelFlag != "":
		cfg.Model.Name = *modelFlag
	case *providerFlag == "anthropic-vertex" || *providerFlag == "anthropic":
		// The config default is a Gemini model.
		cfg.Model.Name = "claude-sonnet-5"
	}
	provider, err := models.Resolve(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "approvereval: resolve provider:", err)
		return 1
	}
	ctx := context.Background()
	llm, err := provider.Model(ctx, cfg.Model.Name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "approvereval: build model:", err)
		return 1
	}
	a, err := approver.New(approver.Options{Model: llm, ModelID: cfg.Model.Name, Timeout: *timeout})
	if err != nil {
		fmt.Fprintln(os.Stderr, "approvereval:", err)
		return 1
	}

	fmt.Printf("approver %s via %s, %d cases\n", cfg.Model.Name, *providerFlag, len(cases))
	var results []approvereval.Result
	started := time.Now()
	stopped := ""
	consecutive := 0
	var inTok, outTok int64
	for i, c := range cases {
		if *deadline > 0 && time.Since(started) > *deadline {
			stopped = fmt.Sprintf("deadline of %s reached after %d of %d cases", *deadline, i, len(cases))
			break
		}
		if *breaker > 0 && consecutive >= *breaker {
			stopped = fmt.Sprintf("%d cases in a row got no answer from the model; stopped after %d of %d", consecutive, i, len(cases))
			break
		}
		if i > 0 && *pace > 0 {
			time.Sleep(*pace)
		}
		var (
			o approvereval.Outcome
			r approvereval.Result
		)
		for attempt := 0; ; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Duration(attempt) * *backoff)
			}
			o, err = approvereval.Run(ctx, a, c)
			if err != nil {
				fmt.Fprintf(os.Stderr, "approvereval: %s: %v\n", c.ID, err)
				return 1
			}
			r = approvereval.GradeCase(c, o)
			inTok += int64(o.InputTokens)
			outTok += int64(o.OutputTokens)
			// Only a case the model gave no answer to is retried. An
			// answer, right or wrong, is the measurement.
			if r.Grade != approvereval.Unmeasured || attempt >= *retries {
				break
			}
			fmt.Printf("  %s: no answer (%s); retrying\n", c.ID, firstLine(r.Error))
		}
		results = append(results, r)
		if r.Grade == approvereval.Unmeasured {
			consecutive++
		} else {
			consecutive = 0
		}
		line := fmt.Sprintf("[%-14s] %-13s %-36s verdict=%-8s %5dms", r.Grade, c.Class, c.ID, r.Verdict, o.Elapsed.Milliseconds())
		if r.Error != "" {
			line += "  error: " + firstLine(r.Error)
		} else if r.Reason != "" {
			line += "  reason: " + r.Reason
		}
		fmt.Println(line)
	}
	rep := approvereval.Summarize(results)
	rep.Model = cfg.Model.Name
	// Every attempt's spend, retries included; Summarize sees only the
	// final attempt of each case.
	rep.InputTokens, rep.OutputTokens = inTok, outTok
	fmt.Printf("\n%s\ntokens: %d in, %d out (all attempts)\n", rep, rep.InputTokens, rep.OutputTokens)
	if stopped != "" {
		fmt.Printf("INDETERMINATE: %s\n", stopped)
	}

	if *jsonOut != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err == nil {
			err = os.WriteFile(*jsonOut, b, 0o644)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "approvereval: write report:", err)
			return 1
		}
	}
	if rep.Verdict() == "fail" {
		return 1
	}
	if stopped != "" {
		return 2
	}
	switch rep.Verdict() {
	case "pass":
		return 0
	case "indeterminate":
		return 2
	default:
		return 1
	}
}

// firstLine is s up to its first newline, so a provider's multi-line
// error body stays on the case's row.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
