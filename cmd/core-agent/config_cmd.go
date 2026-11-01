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
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/go-steer/core-agent/v3/pkg/config"
)

func runConfigSubcommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printConfigUsage(stderr)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	switch args[0] {
	case "import-workload":
		return runConfigImportWorkload(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "core-agent config: unknown subcommand %q (want import-workload)\n", args[0])
		printConfigUsage(stderr)
		return 2
	}
}

func printConfigUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  core-agent config import-workload <workload-dir> [--out <recipe-dir>]")
}

func runConfigImportWorkload(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("core-agent config import-workload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	outDir := fs.String("o", "", "output recipe directory (defaults to <workload-dir>)")
	fs.StringVar(outDir, "out", "", "output recipe directory (defaults to <workload-dir>)")
	if err := fs.Parse(reorderFlagArgs(args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "core-agent config import-workload: expected exactly 1 <workload-dir> argument")
		printConfigUsage(stderr)
		return 2
	}
	workloadDir := strings.TrimSpace(fs.Arg(0))
	targetDir := strings.TrimSpace(*outDir)
	if targetDir == "" {
		targetDir = workloadDir
	}
	cfg, err := config.ImportWorkload(workloadDir, targetDir)
	if err != nil {
		fmt.Fprintf(stderr, "core-agent config import-workload: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "imported workload %q -> %s (%d subagent(s))\n",
		cfg.Agent.DisplayName, filepath.Clean(targetDir), len(cfg.Subagents))
	return 0
}

// reorderFlagArgs places flag arguments (-o/--out and help flags) ahead
// of positional arguments so `import-workload <workload-dir> --out <dir>`
// and `import-workload --out <dir> <workload-dir>` both parse cleanly.
func reorderFlagArgs(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			if (a == "-o" || a == "--o" || a == "-out" || a == "--out") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}
