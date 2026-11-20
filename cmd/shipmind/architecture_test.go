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
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const (
	shipmindModulePrefix        = "github.com/go-steer/shipmind/"
	tailLineThreshold           = 150
	maxNonTestFuncsOver150Lines = 3 // scenario.Run, deploy.Up, (*env.Env).check
)

// packageLayer defines strict acyclic architecture layers across shipmind
// (higher layers may only import strictly lower layers).
var packageLayer = map[string]int{
	"internal/archive":   0,
	"internal/bom":       0,
	"internal/cluster":   0,
	"internal/env":       0,
	"internal/judge":     0,
	"internal/render":    0,
	"internal/secrets":   0,
	"internal/agent":     1,
	"internal/manifests": 1,
	"internal/overlay":   1,
	"internal/scenario":  1,
	"internal/slackapp":  1,
	"internal/deploy":    2,
	"cmd/shipmind":       3,
}

var (
	designHeadingRe  = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s+\{#([A-Z][A-Z0-9]*-[0-9]+)\}\s*$`)
	badBraceRe       = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s+\{#([^}]*)\}\s*$`)
	designBaselineRe = regexp.MustCompile(`^\s*<!--\s*design-baseline:\s*(.*?)\s*-->\s*$`)
	designHexRevRe   = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
)

type commonMarkFence struct {
	inFence   bool
	fenceChar byte
	fenceLen  int
}

func (f *commonMarkFence) advance(trimmed string) {
	if len(trimmed) < 3 || (trimmed[0] != '`' && trimmed[0] != '~') {
		return
	}
	ch := trimmed[0]
	n := 0
	for n < len(trimmed) && trimmed[n] == ch {
		n++
	}
	if n < 3 {
		return
	}
	if !f.inFence {
		f.inFence, f.fenceChar, f.fenceLen = true, ch, n
	} else if ch == f.fenceChar && n >= f.fenceLen && strings.TrimSpace(trimmed[n:]) == "" {
		f.inFence, f.fenceChar, f.fenceLen = false, 0, 0
	}
}

func TestArchitectureLayeringComplexityTailAndDesignAnchors(t *testing.T) {
	t.Parallel()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	verifyShipmindDesignAnchors(t, repoRoot)

	fset := token.NewFileSet()
	var over150 []string

	err = filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == ".worktrees" || name == ".claude" || name == "bin" || name == "dist" || name == "deploy" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(rel, "_test.go") {
			return nil
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, path, raw, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}

		pkgRel := filepath.ToSlash(filepath.Dir(rel))
		verifyPackageImports(t, rel, pkgRel, f)

		lines := strings.Split(string(raw), "\n")
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			start := fset.Position(fn.Pos()).Line
			end := fset.Position(fn.End()).Line
			if countCodeLines(lines, start, end) > tailLineThreshold {
				over150 = append(over150, rel+":"+fn.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(over150) > maxNonTestFuncsOver150Lines {
		t.Errorf("non-test functions over %d code lines = %d (ratchet ceiling %d): %v",
			tailLineThreshold, len(over150), maxNonTestFuncsOver150Lines, over150)
	}
}

func TestDesignAnchorAndTrailerValidatorsRejectInvalidInputs(t *testing.T) {
	t.Parallel()

	// 1. Missing anchors rejected.
	errs, _ := validateDesignMarkdown("docs/DESIGN.md", "# Empty\n## Unanchored\n")
	if len(errs) != 1 || !strings.Contains(errs[0], "zero {#") {
		t.Fatalf("unanchored doc errors = %v, want zero-anchors error", errs)
	}

	// 2. Duplicate ID, malformed ID, and unknown baseline ID rejected; fenced heading ignored.
	badDoc := "# Design\n" +
		"<!-- design-baseline: SM-1, SM-99 -->\n" +
		"````markdown\n```text\n### Ignored inside nested fence {#sm-bad}\n```\n````\n" +
		"### Valid {#SM-1}\n" +
		"### Duplicate {#SM-1}\n" +
		"### Lowercase {#sm-2}\n"
	errs, ids := validateDesignMarkdown("docs/DESIGN.md", badDoc)
	if len(errs) != 3 {
		t.Fatalf("badDoc errors = %v (len %d), want 3 (duplicate, malformed, unknown baseline)", errs, len(errs))
	}

	// 2b. Cross-document duplicate anchor IDs rejected across docs/DESIGN.md and docs/*-design.md.
	crossErrs := checkCrossDocDuplicateIDs(map[string]map[string]int{
		"docs/DESIGN.md":         {"SM-1": 10},
		"docs/console-design.md": {"CON-1": 5, "SM-1": 12},
	})
	if len(crossErrs) != 1 || !strings.Contains(crossErrs[0], "SM-1") {
		t.Fatalf("cross-doc duplicate errors = %v, want 1 error on SM-1", crossErrs)
	}

	// 3. Commit trailer validator: nested 4-backtick fence and indented examples ignored;
	// normalized ./docs/DESIGN.md#SM-1, companion docs/console-design.md#CON-1, and Design-Rev: self accepted;
	// dangling Design-Item, still-present Design-Removed, malformed trailer, and unreachable Design-Rev rejected.
	rawLog := "\x1e11111111deadbeef\x1ffeat: valid with nested fence\n\n" +
		"````text\n```markdown\nDesign-Item: docs/DESIGN.md#FAKE-1\n```\n````\n" +
		"  Design-Item: docs/DESIGN.md#IND-1\n\n" +
		"Design-Item: ./docs/DESIGN.md#SM-1\n" +
		"Design-Item: docs/console-design.md#CON-1\n" +
		"Design-Rev: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
		"\x1e33333333feedface\x1ffeat: valid self design rev\n\n" +
		"Design-Item: docs/console-design.md#CON-1\n" +
		"Design-Rev: SELF\n" +
		"\x1e22222222cafebabe\x1ffeat: invalid trailers\n\n" +
		"Design-Item: docs/DESIGN.md#SM-404\n" +
		"Design-Item: docs/console-design.md#CON-99\n" +
		"Design-Item: malformed-without-hash\n" +
		"Design-Removed: docs/DESIGN.md#SM-1\n" +
		"Design-Rev: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"

	docMap := map[string]map[string]int{
		"docs/DESIGN.md":         ids,
		"docs/console-design.md": {"CON-1": 42},
	}
	tErrs := validateCommitTrailerLog(rawLog, docMap, func(rev string) bool {
		return rev == "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	})
	if len(tErrs) != 5 {
		t.Fatalf("trailer errors = %v (len %d), want 5 (two unknown anchors, malformed, still-present removed, unreachable rev)", tErrs, len(tErrs))
	}
	for _, e := range tErrs {
		if strings.Contains(e, "11111111") || strings.Contains(e, "33333333") {
			t.Fatalf("valid commit falsely flagged: %s", e)
		}
	}
}

func verifyPackageImports(t *testing.T, relFile, pkgRel string, f *ast.File) {
	t.Helper()
	srcLayer, ok := packageLayer[pkgRel]
	if !ok {
		t.Errorf("%s: package %q is not registered in packageLayer architecture map", relFile, pkgRel)
		return
	}
	for _, imp := range f.Imports {
		impPath := strings.Trim(imp.Path.Value, `"`)
		if !strings.HasPrefix(impPath, shipmindModulePrefix) {
			continue
		}
		dstPkg := strings.TrimPrefix(impPath, shipmindModulePrefix)
		dstLayer, known := packageLayer[dstPkg]
		if !known {
			t.Errorf("%s: imports unregistered internal package %q", relFile, dstPkg)
			continue
		}
		if dstLayer >= srcLayer {
			t.Errorf("layering violation in %s (%s, layer %d): cannot import %s (layer %d)",
				relFile, pkgRel, srcLayer, dstPkg, dstLayer)
		}
	}
}

func countCodeLines(lines []string, startLine, endLine int) int {
	n := 0
	for i := startLine - 1; i < endLine && i < len(lines); i++ {
		s := strings.TrimSpace(lines[i])
		if s == "" || strings.HasPrefix(s, "//") {
			continue
		}
		n++
	}
	return n
}

func verifyShipmindDesignAnchors(t *testing.T, repoRoot string) {
	t.Helper()
	relDocs, err := discoverDesignDocs(repoRoot)
	if err != nil {
		t.Fatalf("discover design docs: %v", err)
	}
	if !slices.Contains(relDocs, "docs/DESIGN.md") {
		t.Fatalf("docs/DESIGN.md missing from discovered design docs %v", relDocs)
	}
	docIDs := make(map[string]map[string]int, len(relDocs))
	for _, rel := range relDocs {
		raw, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		errs, ids := validateDesignMarkdown(rel, string(raw))
		for _, e := range errs {
			t.Error(e)
		}
		docIDs[rel] = ids
	}
	for _, e := range checkCrossDocDuplicateIDs(docIDs) {
		t.Error(e)
	}
	verifyDesignCommitTrailers(t, repoRoot, docIDs)
}

func discoverDesignDocs(repoRoot string) ([]string, error) {
	seen := make(map[string]bool)
	addFile := func(abs string) error {
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, abs)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		seen[rel] = true
		return nil
	}

	if err := addFile(filepath.Join(repoRoot, "DESIGN.md")); err != nil {
		return nil, err
	}
	for _, pat := range []string{
		filepath.Join(repoRoot, "design", "*.md"),
		filepath.Join(repoRoot, "docs", "design", "*.md"),
	} {
		matches, err := filepath.Glob(pat)
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			if err := addFile(m); err != nil {
				return nil, err
			}
		}
	}
	docsEntries, err := os.ReadDir(filepath.Join(repoRoot, "docs"))
	if err == nil {
		for _, d := range docsEntries {
			name := d.Name()
			if !d.IsDir() && strings.HasSuffix(strings.ToLower(name), ".md") && strings.Contains(strings.ToLower(name), "design") {
				if err := addFile(filepath.Join(repoRoot, "docs", name)); err != nil {
					return nil, err
				}
			}
		}
	}

	out := make([]string, 0, len(seen))
	for rel := range seen {
		out = append(out, rel)
	}
	slices.Sort(out)
	return out, nil
}

func checkCrossDocDuplicateIDs(docIDs map[string]map[string]int) []string {
	docs := make([]string, 0, len(docIDs))
	for doc := range docIDs {
		docs = append(docs, doc)
	}
	slices.Sort(docs)

	type loc struct {
		doc  string
		line int
	}
	firstSeen := make(map[string]loc)
	var errs []string
	for _, doc := range docs {
		ids := make([]string, 0, len(docIDs[doc]))
		for id := range docIDs[doc] {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			line := docIDs[doc][id]
			if prev, dup := firstSeen[id]; dup {
				errs = append(errs, fmt.Sprintf("%s:%d requirement anchor {#%s} collides with %s:%d", doc, line, id, prev.doc, prev.line))
			} else {
				firstSeen[id] = loc{doc: doc, line: line}
			}
		}
	}
	return errs
}

func validateDesignMarkdown(relPath, content string) ([]string, map[string]int) {
	ids := make(map[string]int)
	var baselines []string
	var errs []string
	var fence commonMarkFence

	for idx, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimRight(rawLine, "\r")
		fence.advance(strings.TrimSpace(line))
		if fence.inFence {
			continue
		}
		if m := designBaselineRe.FindStringSubmatch(line); m != nil {
			for _, tok := range strings.FieldsFunc(m[1], func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
				if id := strings.TrimPrefix(strings.TrimSpace(tok), "#"); id != "" {
					baselines = append(baselines, id)
				}
			}
		}
		if !strings.HasPrefix(line, "#") {
			continue
		}
		if m := designHeadingRe.FindStringSubmatch(line); m != nil {
			id := m[3]
			if prev, dup := ids[id]; dup {
				errs = append(errs, fmt.Sprintf("%s:%d duplicate requirement anchor {#%s} (first at line %d)", relPath, idx+1, id, prev))
			} else {
				ids[id] = idx + 1
			}
			continue
		}
		if bad := badBraceRe.FindStringSubmatch(line); bad != nil {
			errs = append(errs, fmt.Sprintf("%s:%d malformed requirement anchor {#%s} (expected {#PREFIX-N})", relPath, idx+1, bad[3]))
		}
	}
	if len(ids) == 0 {
		errs = append(errs, fmt.Sprintf("%s has zero {#PREFIX-N} requirement anchors", relPath))
	}
	for _, b := range baselines {
		if _, exists := ids[b]; !exists {
			errs = append(errs, fmt.Sprintf("%s design-baseline references unknown anchor #%s", relPath, b))
		}
	}
	return errs, ids
}

func verifyDesignCommitTrailers(t *testing.T, repoRoot string, docIDs map[string]map[string]int) {
	t.Helper()
	if _, ok := runGitReadOnly(repoRoot, "rev-parse", "--verify", "--quiet", "origin/main^{commit}"); !ok {
		return
	}
	rawLog, ok := runGitReadOnly(repoRoot, "log", "--format=\x1e%H\x1f%B", "--end-of-options", "origin/main..HEAD")
	if !ok {
		return
	}
	shallowOut, _ := runGitReadOnly(repoRoot, "rev-parse", "--is-shallow-repository")
	isShallow := strings.TrimSpace(shallowOut) == "true"

	errs := validateCommitTrailerLog(rawLog, docIDs, func(rev string) bool {
		if isShallow {
			return true
		}
		_, exists := runGitReadOnly(repoRoot, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
		return exists
	})
	for _, e := range errs {
		t.Error(e)
	}
}

func validateCommitTrailerLog(
	rawLog string,
	docIDs map[string]map[string]int,
	revReachable func(string) bool,
) []string {
	var errs []string
	for _, rec := range strings.Split(rawLog, "\x1e") {
		rec = strings.TrimSpace(rec)
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, "\x1f", 2)
		if len(parts) != 2 {
			continue
		}
		sha := parts[0]
		if len(sha) > 8 {
			sha = sha[:8]
		}
		var rev string
		hasItem := false
		var fence commonMarkFence

		for _, rawLine := range strings.Split(parts[1], "\n") {
			line := strings.TrimRight(rawLine, "\r")
			fence.advance(strings.TrimSpace(line))
			if fence.inFence || line != strings.TrimLeft(line, " \t") {
				continue
			}
			switch {
			case strings.HasPrefix(line, "Design-Rev:"):
				rev = strings.TrimSpace(strings.TrimPrefix(line, "Design-Rev:"))
			case strings.HasPrefix(line, "Design-Item:"):
				hasItem = true
				doc, id, ok := parseTrailerDocAnchor(strings.TrimPrefix(line, "Design-Item:"))
				if !ok {
					errs = append(errs, fmt.Sprintf("commit %s has malformed Design-Item trailer %q", sha, line))
					continue
				}
				if _, exists := docIDs[doc][id]; !exists {
					errs = append(errs, fmt.Sprintf("commit %s Design-Item references unknown anchor %s#%s", sha, doc, id))
				}
			case strings.HasPrefix(line, "Design-Removed:"):
				doc, id, ok := parseTrailerDocAnchor(strings.TrimPrefix(line, "Design-Removed:"))
				if !ok {
					errs = append(errs, fmt.Sprintf("commit %s has malformed Design-Removed trailer %q", sha, line))
					continue
				}
				if _, stillHere := docIDs[doc][id]; stillHere {
					errs = append(errs, fmt.Sprintf("commit %s declares Design-Removed: %s#%s, but {#%s} is still present", sha, doc, id, id))
				}
			}
		}
		validRev := strings.EqualFold(rev, "self") || (designHexRevRe.MatchString(rev) && (revReachable == nil || revReachable(rev)))
		if hasItem && !validRev {
			errs = append(errs, fmt.Sprintf("commit %s has Design-Item trailer without reachable hex Design-Rev (or Design-Rev: self)", sha))
		}
	}
	return errs
}

func parseTrailerDocAnchor(raw string) (doc, id string, ok bool) {
	s := strings.TrimSpace(raw)
	hash := strings.LastIndexByte(s, '#')
	if hash <= 0 || hash+1 >= len(s) {
		return "", "", false
	}
	return strings.TrimPrefix(s[:hash], "./"), strings.TrimSpace(s[hash+1:]), true
}

func runGitReadOnly(repoRoot string, args ...string) (string, bool) {
	cmd := exec.Command("git", append([]string{"-C", repoRoot}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", false
	}
	return out.String(), true
}
