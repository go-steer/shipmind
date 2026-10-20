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

// Command shipmind works with the platform's pinned component set.
//
//	shipmind check    [-f versions.yaml]   check every pin, exit 1 on a bad one
//	shipmind versions [-f versions.yaml]   list the components and their pins
//
// Deploying (up, down, status), vendoring manifests, bumping a pin and
// running the end-to-end scenarios arrive with Milestone 1; see
// docs/DESIGN.md.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/go-steer/shipmind/internal/bom"
)

const usage = `usage: shipmind <command> [-f versions.yaml]

commands:
  check     check every pin in versions.yaml
  versions  list the components and their pins
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "check", "versions":
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "shipmind: unknown command %q\n\n%s", cmd, usage)
		return 2
	}

	fs := flag.NewFlagSet("shipmind "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("f", "versions.yaml", "path to versions.yaml")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "shipmind %s: unexpected argument %q\n", cmd, fs.Arg(0))
		return 2
	}

	f, err := bom.Load(*path)
	if err != nil {
		fmt.Fprintf(stderr, "shipmind: %v\n", err)
		return 1
	}
	if err := f.Check(); err != nil {
		fmt.Fprintf(stderr, "shipmind: %s:\n", *path)
		for line := range strings.SplitSeq(err.Error(), "\n") {
			fmt.Fprintf(stderr, "  %s\n", line)
		}
		return 1
	}

	switch cmd {
	case "check":
		fmt.Fprintf(stdout, "%s: %d components, all pinned\n", *path, len(f.Components))
	case "versions":
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "COMPONENT\tVERSION\tSOURCE\tPROFILES\tIMAGES")
		for _, c := range f.Components {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n",
				c.Name, c.Version, c.Source, strings.Join(c.Profiles, ","), len(c.Images))
		}
		_ = tw.Flush()
	}
	return 0
}
