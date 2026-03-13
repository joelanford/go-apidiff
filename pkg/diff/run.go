/*
Copyright 2019 Joe Lanford.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package diff

import (
	"bufio"
	"fmt"
	"go/types"
	"os"
	"path/filepath"
	pathpkg "path"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"golang.org/x/exp/apidiff"
	"golang.org/x/tools/go/packages"

	"github.com/joelanford/go-apidiff/pkg/diff/internal/osfs"
)

type Options struct {
	RepoPath       string
	OldCommit      string
	NewCommit      string
	CompareImports bool
	ExcludePaths   []string
}

func Run(opts Options) (*Diff, error) {
	repo, err := git.PlainOpen(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repo: %w", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("failed to get git worktree: %w", err)
	}

	// TODO: Using a custom filesystem is necessary due to a bug related
	//  to computing hashes for symlinks with targets outside the repo.
	//  See: https://github.com/go-git/go-git/issues/253
	wt.Filesystem, err = osfs.New(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to set worktree filesystem interface: %v", err)
	}

	rootFS, err := osfs.New("/")
	if err != nil {
		return nil, fmt.Errorf("failed to create root filesystem interface: %v", err)
	}

	globalIgnores, err := gitignore.LoadGlobalPatterns(rootFS)
	if err != nil {
		return nil, fmt.Errorf("failed to load global gitignore: %v", err)
	}
	wt.Excludes = append(wt.Excludes, globalIgnores...)

	systemIgnores, err := gitignore.LoadSystemPatterns(rootFS)
	if err != nil {
		return nil, fmt.Errorf("failed to load system gitignore: %v", err)
	}
	wt.Excludes = append(wt.Excludes, systemIgnores...)

	if stat, err := wt.Status(); err != nil {
		return nil, fmt.Errorf("failed to get git status: %w", err)
	} else if !stat.IsClean() {
		return nil, &GitStatusError{stat, fmt.Errorf("current git tree is dirty")}
	}

	origRef, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get current HEAD reference: %w", err)
	}

	oldHash, newHash, err := getHashes(repo, plumbing.Revision(opts.OldCommit), plumbing.Revision(opts.NewCommit))
	if err != nil {
		return nil, fmt.Errorf("failed to lookup git commit hashes: %w", err)
	}

	defer func() {
		if err := checkoutRef(*wt, *origRef); err != nil {
			fmt.Printf("WARNING: failed to checkout your original working commit after diff: %v\n", err)
		}
	}()

	selfOld, importsOld, err := getPackages(*wt, *oldHash, opts.ExcludePaths)
	if err != nil {
		return nil, fmt.Errorf("failed to get packages from old commit %q (%s): %w", opts.OldCommit, oldHash, err)
	}

	selfNew, importsNew, err := getPackages(*wt, *newHash, opts.ExcludePaths)
	if err != nil {
		return nil, fmt.Errorf("failed to get packages from new commit %q (%s): %w", opts.NewCommit, newHash, err)
	}

	diff := &Diff{}
	diff.selfReports, diff.selfIncompatible = compareChangesAdditionsAndRemovals(selfOld, selfNew)

	if opts.CompareImports {
		// When comparing imports, we only compare the changes and additions
		// between oldPkgs and newPkgs. We ignore removals in newPkgs because
		// the removed packages are no longer dependencies and therefore have
		// no impact on compatibility of imports.
		diff.importsReports, diff.importsIncompatible = compareChangesAndAdditions(importsOld, importsNew)
	}

	return diff, nil
}

type GitStatusError struct {
	Stat git.Status
	Err  error
}

func (err *GitStatusError) Error() string {
	return fmt.Sprintf("%v\n%v", err.Err, err.Stat)
}

func compareChangesAdditionsAndRemovals(oldPkgs, newPkgs map[string]*packages.Package) (map[string]apidiff.Report, bool) {
	reports, incompatible := compareChangesAndAdditions(oldPkgs, newPkgs)

	// remove packages from oldPkgs that are present in newPkgs. When this loop
	// completes, the packages left in oldPkgs are the ones that were removed
	// and no longer used in the new commit of this repo.
	//
	// This is required for the next loop to be able to report correctly on
	// removes between the old commit and new commit.
	for k := range newPkgs {
		delete(oldPkgs, k)
	}

	for k, oldPackage := range oldPkgs {
		report := apidiff.Changes(oldPackage.Types, types.NewPackage(k, oldPackage.Name))
		for _, c := range report.Changes {
			if !c.Compatible {
				incompatible = true
			}
		}
		reports[k] = report
	}
	return reports, incompatible
}

func compareChangesAndAdditions(oldPkgs, newPkgs map[string]*packages.Package) (map[string]apidiff.Report, bool) {
	reports := map[string]apidiff.Report{}
	incompatible := false
	for k, newPackage := range newPkgs {

		// if this is a brand new package, use a dummy empty package for
		// oldPackage, so that everything in newPackage is reported as new.
		oldPackage, ok := oldPkgs[k]
		if !ok {
			oldPackage = &packages.Package{Types: types.NewPackage(newPackage.PkgPath, newPackage.Name)}
		}

		report := apidiff.Changes(oldPackage.Types, newPackage.Types)
		for _, c := range report.Changes {
			if !c.Compatible {
				incompatible = true
			}
		}
		reports[k] = report
	}
	return reports, incompatible
}

func getHashes(repo *git.Repository, oldRev, newRev plumbing.Revision) (*plumbing.Hash, *plumbing.Hash, error) {
	oldCommitHash, err := repo.ResolveRevision(oldRev)
	if err != nil {
		return nil, nil, fmt.Errorf("could not get hash for %q: %v", oldRev, err)
	}

	newCommitHash, err := repo.ResolveRevision(newRev)
	if err != nil {
		return nil, nil, fmt.Errorf("could not get hash for %q: %v", newRev, err)
	}

	return oldCommitHash, newCommitHash, nil
}

func getPackages(wt git.Worktree, hash plumbing.Hash, excludePaths []string) (map[string]*packages.Package, map[string]*packages.Package, error) {
	if err := wt.Checkout(&git.CheckoutOptions{Hash: hash, Force: true}); err != nil {
		return nil, nil, err
	}
	if err := wt.Clean(&git.CleanOptions{Dir: true}); err != nil {
		return nil, nil, err
	}
	if err := wt.Reset(&git.ResetOptions{Commit: hash, Mode: git.HardReset}); err != nil {
		return nil, nil, err
	}

	modulePath, err := getModulePath(wt)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read module path: %w", err)
	}

	goFlags := "-mod=readonly"
	if st, err := os.Stat(filepath.Join(wt.Filesystem.Root(), "vendor")); err == nil && st.IsDir() {
		goFlags = "-mod=vendor"
	}
	cfg := packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedTypes | packages.NeedTypesSizes,
		Tests:      false,
		BuildFlags: []string{goFlags},
	}
	pkgs, err := packages.Load(&cfg, "./...")
	if err != nil {
		return nil, nil, err
	}

	selfPkgs := make(map[string]*packages.Package)
	importPkgs := make(map[string]*packages.Package)
	for _, pkg := range pkgs {
		// skip internal packages since they do not contain public APIs
		if strings.HasSuffix(pkg.PkgPath, "/internal") || strings.Contains(pkg.PkgPath, "/internal/") {
			continue
		}
		if isExcluded(pkg.PkgPath, modulePath, excludePaths) {
			continue
		}
		selfPkgs[pkg.PkgPath] = pkg
	}
	for _, pkg := range pkgs {
		for _, ipkg := range pkg.Imports {
			if _, ok := selfPkgs[ipkg.PkgPath]; !ok {
				importPkgs[ipkg.PkgPath] = ipkg
			}
		}
	}

	// Reset the worktree. Sometimes loading the packages can cause the
	// worktree to become dirty. It seems like this occurs because package
	// loading can change go.mod and go.sum.
	//
	// TODO(joelanford): If go-git starts to support checking out of specific
	//   files we can update this to be less aggressive and only checkout
	//   go.mod and go.sum instead of resetting the entire tree.
	if err := wt.Reset(&git.ResetOptions{
		Mode:   git.HardReset,
		Commit: hash,
	}); err != nil {
		return nil, nil, fmt.Errorf("failed to hard reset to %v: %w", hash, err)
	}

	return selfPkgs, importPkgs, nil
}

func checkoutRef(wt git.Worktree, ref plumbing.Reference) (err error) {
	if ref.Name() == "HEAD" {
		return wt.Checkout(&git.CheckoutOptions{Hash: ref.Hash()})
	}
	return wt.Checkout(&git.CheckoutOptions{Branch: ref.Name()})
}

// getModulePath reads go.mod from the worktree root and returns the module path.
func getModulePath(wt git.Worktree) (string, error) {
	goModPath := filepath.Join(wt.Filesystem.Root(), "go.mod")
	f, err := os.Open(goModPath)
	if err != nil {
		return "", fmt.Errorf("failed to open go.mod: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module ")), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading go.mod: %w", err)
	}
	return "", fmt.Errorf("module directive not found in go.mod")
}

// isExcluded checks whether a package path should be excluded based on the
// configured exclude patterns. It strips the module prefix from the package
// path to get a module-relative path, then checks each pattern.
func isExcluded(pkgPath, modulePath string, excludePaths []string) bool {
	if len(excludePaths) == 0 {
		return false
	}

	// Get the path relative to the module root
	relPath := pkgPath
	if pkgPath == modulePath {
		relPath = "."
	} else if strings.HasPrefix(pkgPath, modulePath+"/") {
		relPath = strings.TrimPrefix(pkgPath, modulePath+"/")
	} else {
		// Not a module package, don't exclude
		return false
	}

	for _, pattern := range excludePaths {
		if matchPattern(pattern, relPath) {
			return true
		}
	}
	return false
}

// matchPattern checks if path matches the given pattern. Patterns are split
// by "/" and matched segment-by-segment. "*" matches any single path segment
// (using path.Match), "**" matches zero or more complete path segments.
// A pattern without wildcards does prefix matching: "cmd" matches "cmd",
// "cmd/tool", "cmd/tool/sub", etc.
func matchPattern(pattern, path string) bool {
	// Patterns without wildcards do prefix matching
	if !strings.ContainsAny(pattern, "*?[") {
		return path == pattern || strings.HasPrefix(path, pattern+"/")
	}

	patternParts := strings.Split(pattern, "/")
	pathParts := strings.Split(path, "/")

	return matchParts(patternParts, pathParts)
}

func matchParts(patternParts, pathParts []string) bool {
	if len(patternParts) == 0 {
		return len(pathParts) == 0
	}

	seg := patternParts[0]

	if seg == "**" {
		restPattern := patternParts[1:]
		// When "**" is the last pattern element (no following segments),
		// it requires at least one path segment (e.g., cmd/** does not
		// match cmd itself). Otherwise, it matches zero or more segments
		// (e.g., **/generated matches generated at root).
		start := 0
		if len(restPattern) == 0 {
			start = 1
		}
		for i := start; i <= len(pathParts); i++ {
			if matchParts(restPattern, pathParts[i:]) {
				return true
			}
		}
		return false
	}

	if len(pathParts) == 0 {
		return false
	}

	matched, err := pathpkg.Match(seg, pathParts[0])
	if err != nil || !matched {
		return false
	}

	return matchParts(patternParts[1:], pathParts[1:])
}
