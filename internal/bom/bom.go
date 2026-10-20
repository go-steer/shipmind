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

// Package bom reads and checks versions.yaml, the set of component
// versions a shipmind release pins.
//
// The file is the platform's bill of materials: every component, the
// repository it is built from, the exact version of that repository,
// the deployment profiles that run it, and the images it ships. A
// version is a release tag (vX.Y.Z) or a full commit SHA, never a branch
// or an abbreviated SHA (git resolves a branch of the same name first),
// and an image is pinned by digest, never by tag alone. Branches and
// image tags move without anyone editing this file, and a pin that
// moves is not a pin.
package bom

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Profiles are the deployment profiles a component can belong to.
// substrate runs agents as ax tasks on Agent Substrate; standalone runs
// them as plain Kubernetes workloads.
var Profiles = []string{"substrate", "standalone"}

// File is a parsed versions.yaml.
type File struct {
	Components []Component `yaml:"components"`
}

// Component is one pinned piece of the platform.
type Component struct {
	// Name is the component's short name, unique in the file.
	Name string `yaml:"name"`
	// Source is the repository the component is built from, as a
	// module-style path ("github.com/go-steer/mast").
	Source string `yaml:"source"`
	// Version is a release tag (vX.Y.Z, optionally with a pre-release
	// or build suffix) or a full 40-character commit SHA in Source.
	Version string `yaml:"version"`
	// Profiles lists the deployment profiles that run the component.
	Profiles []string `yaml:"profiles"`
	// Images are the container images the component ships, each pinned
	// by digest. Third-party images a component deploys (its database,
	// its proxy) are listed under that component.
	Images []Image `yaml:"images,omitempty"`
	// Note says why the pin is what it is, when that isn't obvious.
	Note string `yaml:"note,omitempty"`
}

// Image is a container image pinned by digest.
type Image struct {
	// Name is the image's role in the component ("ax-server", "redis").
	Name string `yaml:"name"`
	// Ref is the full reference, which must end in @sha256:<digest>.
	// A tag before the digest is allowed and documents intent.
	Ref string `yaml:"ref"`
}

var (
	nameRE    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	versionRE = regexp.MustCompile(`^([0-9a-f]{40}|v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?)$`)
	sourceRE  = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)+/[A-Za-z0-9_][A-Za-z0-9._-]*/[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	digestRE  = regexp.MustCompile(imageRef)
)

// imageRef is the image reference grammar from distribution/reference,
// with the digest required: [domain[:port]/]path[:tag]@sha256:<hex>.
const (
	refDomainPart = `[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?`
	refDomain     = refDomainPart + `(\.` + refDomainPart + `)*(:[0-9]+)?`
	refComponent  = `[a-z0-9]+(([._]|__|-+)[a-z0-9]+)*`
	refPath       = refComponent + `(/` + refComponent + `)*`
	imageRef      = `^(` + refDomain + `/)?` + refPath + `(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?@sha256:[0-9a-f]{64}$`
)

// Load reads and parses the file at path. It does not check it; call
// File.Check for that.
func Load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := Parse(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// Parse decodes a versions.yaml document. Unknown fields are an error,
// so a misspelled key fails here instead of silently pinning nothing.
func Parse(r io.Reader) (*File, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty document")
		}
		return nil, err
	}
	// The decoder reads one document per call, so a second document (or
	// garbage after a `---`) would otherwise never be looked at.
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("after the first document: %w", err)
		}
		return nil, errors.New("more than one document")
	}
	return &f, nil
}

// Check reports every problem in the file at once, joined into one
// error, or nil when the file is sound.
func (f *File) Check() error {
	var errs []error
	if len(f.Components) == 0 {
		errs = append(errs, errors.New("no components"))
	}
	seen := map[string]bool{}
	for i, c := range f.Components {
		label := c.Name
		if label == "" {
			label = fmt.Sprintf("components[%d]", i)
		}
		fail := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("%s: %s", label, fmt.Sprintf(format, args...)))
		}

		switch {
		case c.Name == "":
			fail("no name")
		case !nameRE.MatchString(c.Name):
			fail("name %q is not lowercase letters, digits and dashes", c.Name)
		case seen[c.Name]:
			fail("listed twice")
		}
		seen[c.Name] = true

		if !sourceRE.MatchString(c.Source) {
			fail("source %q is not a repository path like github.com/org/repo", c.Source)
		}
		switch {
		case c.Version == "":
			fail("no version")
		case !versionRE.MatchString(c.Version):
			fail("version %q is not a release tag (vX.Y.Z) or a full 40-character commit SHA", c.Version)
		}
		if len(c.Profiles) == 0 {
			fail("no profiles")
		}
		for _, p := range c.Profiles {
			if !slices.Contains(Profiles, p) {
				fail("unknown profile %q (want one of %s)", p, strings.Join(Profiles, ", "))
			}
		}

		images := map[string]bool{}
		for j, img := range c.Images {
			if img.Name == "" {
				fail("images[%d]: no name", j)
			} else if images[img.Name] {
				fail("image %q listed twice", img.Name)
			}
			images[img.Name] = true
			if !digestRE.MatchString(img.Ref) {
				fail("image %q: %q is not an image reference pinned by digest (want <name>[:tag]@sha256:<64 hex>)", img.Name, img.Ref)
			}
		}
	}
	return errors.Join(errs...)
}

// Component returns the component with the given name.
func (f *File) Component(name string) (Component, bool) {
	for _, c := range f.Components {
		if c.Name == name {
			return c, true
		}
	}
	return Component{}, false
}
