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
//	shipmind status   [-root .] [-any-ref] <env-dir>    compare an environment with its cluster (read only)
//	shipmind up       [-root .] [-dry-run] <env-dir>    bring the cluster to the environment
//	shipmind down     [-root .] -yes [-all] <env-dir>   delete the environment's rendered objects
//
// An environment directory is an overlay: a kustomization.yaml that pins
// a shipmind profile, and a shipmind.yaml (internal/env). The deploy
// commands run from a shipmind checkout (-root) at the pinned commit.
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
	"github.com/go-steer/shipmind/internal/cluster"
	"github.com/go-steer/shipmind/internal/deploy"
	"github.com/go-steer/shipmind/internal/env"
	"github.com/go-steer/shipmind/internal/manifests"
	"github.com/go-steer/shipmind/internal/overlay"
)

// fetcher is how `vendor` reads source repositories; tests replace it.
var fetcher manifests.Fetcher = manifests.Git{}

// newCluster is how the deploy commands reach a cluster; tests replace it.
var newCluster = func(kubeContext string) cluster.Cluster { return cluster.Kube{Context: kubeContext} }

const usage = `usage: shipmind <command> [-f versions.yaml] [args]

commands:
  check     check every pin in versions.yaml and every vendored manifest
  versions  list the components and their pins
  vendor    fetch manifests at their pins into deploy/vendor (all, or the named components)
  status    compare an environment (an overlay directory) with its cluster, read only
  up        bring the cluster to an environment (-dry-run validates without writing)
  down      delete an environment's rendered objects (-yes; keeps namespaces and volumes unless -all)
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
	case "status", "up", "down":
		return runDeploy(cmd, args, stdout, stderr)
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

// Exit codes for status: 0 no drift, 3 drift (so a script can tell drift
// from a failure, which is 1).
const exitDrift = 3

func runDeploy(cmd string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shipmind "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "the shipmind checkout, at the commit the environment pins")
	anyRef := fs.Bool("any-ref", false, "allow a checkout at another commit or with uncommitted changes (status only)")
	dryRun := fs.Bool("dry-run", false, "validate every write server-side and write nothing (up)")
	yes := fs.Bool("yes", false, "confirm deleting (down)")
	all := fs.Bool("all", false, "also delete namespaces and persistent volume claims (down)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "shipmind %s: want one environment directory\n", cmd)
		return 2
	}
	if *anyRef && cmd != "status" {
		fmt.Fprintf(stderr, "shipmind %s: -any-ref is for status only; a deploy uses exactly the pinned commit\n", cmd)
		return 2
	}
	if cmd == "down" && !*yes {
		fmt.Fprintln(stderr, "shipmind down: pass -yes to delete the environment's objects")
		return 2
	}

	dir := fs.Arg(0)
	e, err := env.Load(dir)
	if err != nil {
		fmt.Fprintf(stderr, "shipmind %s: %v\n", cmd, err)
		return 1
	}
	res, err := overlay.Render(dir, *root, overlay.Options{AnyRef: *anyRef})
	if err != nil {
		fmt.Fprintf(stderr, "shipmind %s: %v\n", cmd, err)
		return 1
	}
	plan := &deploy.Plan{Env: e, Root: *root, Objects: res.Objects, Ref: res.Ref}
	c := newCluster(e.Context)
	ctx := context.Background()
	fmt.Fprintf(stdout, "%s: context %s, shipmind %s, %d objects rendered\n", dir, e.Context, res.Ref[:12], len(res.Objects))

	switch cmd {
	case "status":
		r, err := deploy.Status(ctx, plan, c)
		if err != nil {
			fmt.Fprintf(stderr, "shipmind status: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "agent tokens: %s\n", r.Tokens)
		for _, ch := range r.Changes {
			if ch.Expected != "" {
				fmt.Fprintf(stdout, "  expected  %s: %s\n            (%s)\n", ch.What, ch.Detail, ch.Expected)
			} else {
				fmt.Fprintf(stdout, "  DRIFT     %s: %s\n", ch.What, ch.Detail)
			}
		}
		drift := r.Drift()
		fmt.Fprintf(stdout, "%d changes, %d expected, %d drift\n", len(r.Changes), len(r.Changes)-len(drift), len(drift))
		if len(drift) > 0 {
			return exitDrift
		}
	case "up":
		err := deploy.Up(ctx, plan, c, deploy.UpOptions{
			DryRun: *dryRun,
			Log:    func(format string, args ...any) { fmt.Fprintf(stdout, format+"\n", args...) },
		})
		if err != nil {
			fmt.Fprintf(stderr, "shipmind up: %v\n", err)
			return 1
		}
	case "down":
		out, err := deploy.Down(ctx, plan, c, *all)
		fmt.Fprint(stdout, out)
		if err != nil {
			fmt.Fprintf(stderr, "shipmind down: %v\n", err)
			return 1
		}
	}
	return 0
}
