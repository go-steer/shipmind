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

// Package overlay renders an environment overlay against a local
// shipmind checkout.
//
// An overlay references a shipmind profile as a remote kustomize base at
// a full commit SHA. Deploying from it should use exactly that commit,
// and the agent templates `shipmind up` renders come from the checkout
// it runs in, so Render requires the checkout to be at the pinned
// commit, then builds the overlay with the remote base swapped for the
// checkout's copy: the same content, no network.
package overlay

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/go-steer/shipmind/internal/render"
)

// remoteRE matches a shipmind remote base: the repository, a path in it
// and a full commit SHA.
var remoteRE = regexp.MustCompile(`^https://github\.com/go-steer/shipmind//([A-Za-z0-9._/-]+)\?ref=([0-9a-f]{40})$`)

// Options control Render.
type Options struct {
	// AnyRef skips the check that the checkout is at the overlay's pinned
	// commit with a clean tree, for trying an overlay against unmerged
	// shipmind changes. Never for a deploy.
	AnyRef bool
}

// Result is a rendered overlay.
type Result struct {
	Objects render.Objects
	// Ref is the shipmind commit the overlay pins.
	Ref string
}

// Render builds the overlay in dir against the shipmind checkout root.
//
// Every kustomization in the overlay is checked: each entry in
// resources, components and bases must be either a shipmind pin
// (https://github.com/go-steer/shipmind//<path>?ref=<full SHA>, all at
// one SHA), which is swapped for the checkout's copy, or a relative path
// inside the overlay. Anything else would make the render fetch from the
// network or read outside what was reviewed, so it is refused, as are
// helmCharts and kustomization files under another name.
func Render(dir, root string, opts Options) (*Result, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, "kustomization.yaml")); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "shipmind-overlay-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := copyTree(dir, tmp); err != nil {
		return nil, err
	}

	ref := ""
	err = filepath.WalkDir(tmp, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(tmp, p)
		switch d.Name() {
		case "kustomization.yml", "Kustomization":
			return fmt.Errorf("%s: name kustomizations kustomization.yaml", filepath.Join(dir, rel))
		case "kustomization.yaml":
		default:
			return nil
		}
		r, err := rewrite(p, filepath.Join(dir, rel), tmp, root)
		if err != nil {
			return err
		}
		if r != "" {
			if ref != "" && ref != r {
				return fmt.Errorf("%s: pins shipmind at both %s and %s", dir, ref, r)
			}
			ref = r
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if ref == "" {
		return nil, fmt.Errorf("%s: references no shipmind profile", filepath.Join(dir, "kustomization.yaml"))
	}
	if !opts.AnyRef {
		if err := atRef(root, ref); err != nil {
			return nil, err
		}
	}
	objs, err := render.Build(tmp)
	if err != nil {
		return nil, fmt.Errorf("render %s: %w", dir, err)
	}
	return &Result{Objects: objs, Ref: ref}, nil
}

// rewrite checks one kustomization (at p in the temporary copy rooted at
// tmp; shown as name) and swaps its shipmind pins for paths into root.
// It returns the pinned SHA, if the file pins one.
func rewrite(p, name, tmp, root string) (string, error) {
	b, err := os.ReadFile(p) //nolint:gosec // inside our own temporary copy
	if err != nil {
		return "", err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if v := mappingValue(doc.Content, "helmCharts"); v != nil {
		return "", fmt.Errorf("%s: helmCharts fetch at render time; vendor the rendered chart instead", name)
	}
	ref := ""
	for _, key := range []string{"resources", "components", "bases"} {
		list := mappingValue(doc.Content, key)
		if list == nil {
			continue
		}
		if list.Kind != yaml.SequenceNode {
			return "", fmt.Errorf("%s: %s is not a list", name, key)
		}
		for _, n := range list.Content {
			if m := remoteRE.FindStringSubmatch(n.Value); m != nil {
				if slices.Contains(strings.Split(m[1], "/"), "..") {
					return "", fmt.Errorf("%s: %q leaves the shipmind repository", name, n.Value)
				}
				if ref != "" && ref != m[2] {
					return "", fmt.Errorf("%s: pins shipmind at both %s and %s", name, ref, m[2])
				}
				ref = m[2]
				local := filepath.Join(root, filepath.FromSlash(m[1]))
				if _, err := os.Stat(filepath.Join(local, "kustomization.yaml")); err != nil {
					return "", fmt.Errorf("%s: %s is not a kustomization in %s", name, m[1], root)
				}
				rel, err := filepath.Rel(filepath.Dir(p), local)
				if err != nil {
					return "", err
				}
				n.Value = filepath.ToSlash(rel)
				continue
			}
			if !insideOverlay(filepath.Dir(p), n.Value, tmp) {
				return "", fmt.Errorf("%s: %s entry %q is neither a shipmind pin (https://github.com/go-steer/shipmind//<path>?ref=<full commit SHA>) nor a path inside the overlay", name, key, n.Value)
			}
		}
	}
	if ref == "" {
		return "", nil
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", err
	}
	return ref, os.WriteFile(p, out.Bytes(), 0o600)
}

// insideOverlay reports whether entry, relative to dir, is a plain path
// that stays inside the overlay copy at tmp.
func insideOverlay(dir, entry, tmp string) bool {
	if entry == "" || filepath.IsAbs(entry) || strings.Contains(entry, "://") || strings.Contains(entry, "@") ||
		strings.Contains(entry, "?") || strings.Contains(entry, "//") {
		return false
	}
	target := filepath.Clean(filepath.Join(dir, filepath.FromSlash(entry)))
	rel, err := filepath.Rel(tmp, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	_, err = os.Stat(target)
	return err == nil
}

// atRef checks that the checkout at root is at commit ref with no
// uncommitted changes.
func atRef(root, ref string) error {
	head, err := gitOut(root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != ref {
		return fmt.Errorf("the overlay pins shipmind %s, but %s is at %s (check it out, or pass -any-ref to try unmerged changes)", ref, root, head)
	}
	// --untracked-files=all: a user's status.showUntrackedFiles=no must
	// not hide a stray file.
	status, err := gitOut(root, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("%s has uncommitted changes; a deploy uses exactly the pinned commit (or pass -any-ref)", root)
	}
	return nil
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git subcommands; dir is the operator's checkout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s in %s: %w: %s", strings.Join(args, " "), dir, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// mappingValue returns the value node for key in the document's top
// mapping.
func mappingValue(content []*yaml.Node, key string) *yaml.Node {
	if len(content) == 0 || content[0].Kind != yaml.MappingNode {
		return nil
	}
	m := content[0].Content
	for i := 0; i+1 < len(m); i += 2 {
		if m[i].Value == key {
			return m[i+1]
		}
	}
	return nil
}

// copyTree copies the regular files under src into dst, skipping dot
// directories (a .git, say). Symlinks are refused.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file", p)
		}
		b, err := os.ReadFile(p) //nolint:gosec // the overlay is the operator's own directory
		if err != nil {
			return err
		}
		// rel comes from walking src, so it stays inside dst.
		return os.WriteFile(filepath.Join(dst, rel), b, 0o600) //nolint:gosec // see above
	})
}
