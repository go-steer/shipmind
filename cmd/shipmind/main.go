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
//	shipmind check    [-f versions.yaml]                check every pin and every vendored manifest
//	shipmind versions [-f versions.yaml]                list the components and their pins
//	shipmind vendor   [-f versions.yaml] [component...] fetch manifests at their pins into deploy/vendor
//
// Deploying (up, down, status), bumping a pin and running the end-to-end
// scenarios arrive with Milestone 1; see docs/DESIGN.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/go-steer/shipmind/internal/bom"
	"github.com/go-steer/shipmind/internal/manifests"
)

// fetcher is how `vendor` reads source repositories; tests replace it.
var fetcher manifests.Fetcher = manifests.Git{}

const usage = `usage: shipmind <command> [-f versions.yaml] [args]

commands:
  check     check every pin in versions.yaml and every vendored manifest
  versions  list the components and their pins
  vendor    fetch manifests at their pins into deploy/vendor (all, or the named components)
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
	case "check", "versions", "vendor":
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
	if fs.NArg() > 0 && cmd != "vendor" {
		fmt.Fprintf(stderr, "shipmind %s: unexpected argument %q\n", cmd, fs.Arg(0))
		return 2
	}

	f, err := bom.Load(*path)
	if err != nil {
		fmt.Fprintf(stderr, "shipmind: %v\n", err)
		return 1
	}
	if err := f.Check(); err != nil {
		report(stderr, *path, err)
		return 1
	}
	root := filepath.Dir(*path)

	switch cmd {
	case "check":
		errs := []error{manifests.Strays(root, f)}
		vendored := 0
		for _, c := range f.Components {
			errs = append(errs, manifests.Verify(root, c))
			if len(c.Manifests) > 0 {
				vendored++
			}
		}
		if err := errors.Join(errs...); err != nil {
			report(stderr, *path, err)
			return 1
		}
		fmt.Fprintf(stdout, "%s: %d components, all pinned; %d vendored, all unedited\n", *path, len(f.Components), vendored)
	case "vendor":
		names := fs.Args()
		if len(names) == 0 {
			for _, c := range f.Components {
				if len(c.Manifests) > 0 {
					names = append(names, c.Name)
				}
			}
		}
		for _, name := range names {
			c, ok := f.Component(name)
			if !ok {
				fmt.Fprintf(stderr, "shipmind vendor: no component %q\n", name)
				return 1
			}
			if err := manifests.Vendor(context.Background(), fetcher, root, c); err != nil {
				fmt.Fprintf(stderr, "shipmind vendor: %v\n", err)
				return 1
			}
			fmt.Fprintf(stdout, "vendored %s@%s: %d files\n", c.Name, c.Version, len(c.Manifests))
		}
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

func report(w io.Writer, path string, err error) {
	fmt.Fprintf(w, "shipmind: %s:\n", path)
	for line := range strings.SplitSeq(err.Error(), "\n") {
		fmt.Fprintf(w, "  %s\n", line)
	}
}
