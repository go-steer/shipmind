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

package bom

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	sha    = "922e5491ba10562ddd68285e318c3efddf074d59"
)

const good = `
components:
  - name: mast
    source: github.com/go-steer/mast
    version: ` + sha + `
    profiles: [substrate]
  - name: ax
    source: github.com/mastersingh24/ax
    version: v0.5.0-rc.1+fork
    profiles: [substrate]
    images:
      - name: redis
        ref: docker.io/library/redis:7-alpine@` + digest + `
    note: fork of google/ax
`

func mustParse(t *testing.T, doc string) *File {
	t.Helper()
	f, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}

func TestGoodFile(t *testing.T) {
	f := mustParse(t, good)
	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
	ax, ok := f.Component("ax")
	if !ok {
		t.Fatal("Component(ax) not found")
	}
	if ax.Images[0].Name != "redis" || ax.Note != "fork of google/ax" {
		t.Errorf("ax = %+v", ax)
	}
	if _, ok := f.Component("nope"); ok {
		t.Error("Component(nope) found")
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse(strings.NewReader("components:\n  - name: mast\n    branch: main\n"))
	if err == nil || !strings.Contains(err.Error(), "branch") {
		t.Fatalf("err = %v, want unknown field branch", err)
	}
}

func TestParseRejectsEmpty(t *testing.T) {
	_, err := Parse(strings.NewReader(""))
	if err == nil || err.Error() != "empty document" {
		t.Fatalf("err = %v, want empty document", err)
	}
}

func TestParseRejectsMoreDocuments(t *testing.T) {
	for _, tc := range []struct{ name, tail, want string }{
		{"second document", "---\ncomponents:\n  - {name: b, version: main}\n", "more than one document"},
		{"garbage", "---\n: : [ {\n", "after the first document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(good + tc.tail))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestVersions(t *testing.T) {
	for _, v := range []string{sha, "v0.36.0", "v1.0.0-rc.4", "v3.0.0-dev.2", "v0.5.0+gke.1"} {
		f := &File{Components: []Component{{Name: "a", Source: "github.com/a/b", Version: v, Profiles: []string{"substrate"}}}}
		if err := f.Check(); err != nil {
			t.Errorf("version %q: %v", v, err)
		}
	}
	for _, v := range []string{
		"main", "refs/heads/main", "HEAD", " v1.0.0", "v1.0.0 main", "922e549", // abbreviated SHA
		"deployed/substrate-v0.5-685b160", "v1.0", "1.0.0", "v01.0.0", strings.ToUpper(sha), sha + "0",
	} {
		f := &File{Components: []Component{{Name: "a", Source: "github.com/a/b", Version: v, Profiles: []string{"substrate"}}}}
		if err := f.Check(); err == nil || !strings.Contains(err.Error(), "is not a release tag") {
			t.Errorf("version %q: err = %v, want rejected", v, err)
		}
	}
}

func TestSources(t *testing.T) {
	for _, src := range []string{"github.com/go-steer/mast", "gitlab.example.co.uk/a_b/c.d-e"} {
		f := &File{Components: []Component{{Name: "a", Source: src, Version: sha, Profiles: []string{"substrate"}}}}
		if err := f.Check(); err != nil {
			t.Errorf("source %q: %v", src, err)
		}
	}
	for _, src := range []string{"github.com/x", "github.com/../..", "github.com/a/b/c", "go-steer/mast", "https://github.com/a/b", "github.com/a/.b"} {
		f := &File{Components: []Component{{Name: "a", Source: src, Version: sha, Profiles: []string{"substrate"}}}}
		if err := f.Check(); err == nil || !strings.Contains(err.Error(), "is not a repository path") {
			t.Errorf("source %q: err = %v, want rejected", src, err)
		}
	}
}

func TestImageRefs(t *testing.T) {
	for _, ref := range []string{
		"redis@" + digest,
		"docker.io/library/redis:7-alpine@" + digest,
		"localhost:5000/a/b_c__d/e--f:v1.2_3@" + digest,
		"ghcr.io/go-steer/lookout:v0.36.0-gke@" + digest,
	} {
		f := &File{Components: []Component{{Name: "a", Source: "github.com/a/b", Version: sha, Profiles: []string{"substrate"},
			Images: []Image{{Name: "x", Ref: ref}}}}}
		if err := f.Check(); err != nil {
			t.Errorf("ref %q: %v", ref, err)
		}
	}
	for _, ref := range []string{
		"redis:7-alpine",             // tag only
		"redis@sha256:abc",           // short digest
		":@" + digest,                // no name
		"redis:7 junk@" + digest,     // whitespace
		"Redis@" + digest,            // uppercase path
		"redis:-bad@" + digest,       // tag can't start with a dash
		"redis@" + digest + "\n",     // trailing newline
		"redis@sha512:" + digest[7:], // wrong algorithm
	} {
		f := &File{Components: []Component{{Name: "a", Source: "github.com/a/b", Version: sha, Profiles: []string{"substrate"},
			Images: []Image{{Name: "x", Ref: ref}}}}}
		if err := f.Check(); err == nil || !strings.Contains(err.Error(), "pinned by digest") {
			t.Errorf("ref %q: err = %v, want rejected", ref, err)
		}
	}
}

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		want []string
	}{
		{"no components", "components: []\n", []string{"no components"}},
		{"missing fields", "components:\n  - {}\n", []string{
			"components[0]: no name", "is not a repository path", "no version", "no profiles",
		}},
		{"bad name", "components:\n  - {name: Mast, source: github.com/a/b, version: v1.0.0, profiles: [substrate]}\n",
			[]string{`name "Mast"`}},
		{"duplicate", "components:\n  - {name: a, source: github.com/a/b, version: v1.0.0, profiles: [substrate]}\n" +
			"  - {name: a, source: github.com/a/b, version: v1.0.0, profiles: [substrate]}\n",
			[]string{"a: listed twice"}},
		{"unknown profile", "components:\n  - {name: a, source: github.com/a/b, version: v1.0.0, profiles: [cloud-run]}\n",
			[]string{`unknown profile "cloud-run"`}},
		{"tag-only image", "components:\n  - {name: a, source: github.com/a/b, version: v1.0.0, profiles: [substrate],\n" +
			"     images: [{name: redis, ref: 'redis:7-alpine'}]}\n",
			[]string{`image "redis": "redis:7-alpine" is not an image reference pinned by digest`}},
		{"short digest", "components:\n  - {name: a, source: github.com/a/b, version: v1.0.0, profiles: [substrate],\n" +
			"     images: [{name: x, ref: 'x@sha256:abc'}]}\n",
			[]string{"pinned by digest"}},
		{"image problems", "components:\n  - {name: a, source: github.com/a/b, version: v1.0.0, profiles: [substrate],\n" +
			"     images: [{ref: 'x@" + digest + "'}, {name: y, ref: 'y@" + digest + "'}, {name: y, ref: 'y@" + digest + "'}]}\n",
			[]string{"images[0]: no name", `image "y" listed twice`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := mustParse(t, tc.doc).Check()
			if err == nil {
				t.Fatal("Check succeeded")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q\nmissing %q", err, w)
				}
			}
		})
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "versions.yaml")
	if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Components) != 2 {
		t.Errorf("got %d components, want 2", len(f.Components))
	}

	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("Load(missing) succeeded")
	}

	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("components: {"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), bad) {
		t.Errorf("Load(bad) err = %v, want it to name the file", err)
	}
}

// The repository's own versions.yaml is checked here as well as by the
// verify-versions presubmit, so `go test ./...` alone catches a bad pin.
func TestRepoVersionsFile(t *testing.T) {
	f, err := Load("../../versions.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
}
