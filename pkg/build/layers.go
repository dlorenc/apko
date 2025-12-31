// Copyright 2025 Chainguard, Inc.
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

package build

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"slices"

	"chainguard.dev/apko/pkg/apk/apk"
	apkfs "chainguard.dev/apko/pkg/apk/fs"

	"github.com/chainguard-dev/clog"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func (bc *Context) buildLayers(ctx context.Context) ([]v1.Layer, error) {
	log := clog.FromContext(ctx)

	if strategy := bc.ic.Layering.Strategy; strategy != "origin" {
		return nil, fmt.Errorf("unrecognized layering strategy %q", strategy)
	}

	if bc.ic.Contents.BaseImage != nil {
		return nil, fmt.Errorf("layering with %q is unsupported", "baseimage")
	}

	// Check layer cache before building if configured
	if bc.layerCacheConfig != nil && bc.layerCacheConfig.Registry != "" {
		layers, ok, err := bc.tryLayerCache(ctx)
		if err != nil {
			log.Warnf("layer cache check failed, falling back to full build: %v", err)
		} else if ok {
			return layers, nil
		}
		// Partial or no cache hit - continue with full build
	}

	// Build a single fs.FS, the normal way (this writes to bc.fs).
	diffs, err := bc.buildImage(ctx)
	if err != nil {
		return nil, fmt.Errorf("building filesystem: %w", err)
	}

	pkgs := make([]*apk.Package, 0, len(diffs))
	pkgToDiff := map[*apk.Package][]byte{}
	for _, pkgDiff := range diffs {
		pkgs = append(pkgs, pkgDiff.Package)
		pkgToDiff[pkgDiff.Package] = pkgDiff.Diff
	}

	// We don't pass around repositories cleanly between apko and the library
	// formerly known as go-apk. Instead, we write stuff to bc.fs directly
	// and the library formerly known as go-apk reads from bc.fs to know
	// which repositories it can fetch packages from. We need to call this
	// to overwrite etc/apk/repositories with _only_ runtime repositories
	// and not runtime + build repositories.
	//
	// TODO: Clean this up when time permits.
	if err := bc.postBuildSetApk(ctx); err != nil {
		return nil, err
	}

	// Use our layering strategy to partition packages into a set of Budget groups.
	groups, err := groupByOriginAndSize(pkgs, bc.ic.Layering.Budget)
	if err != nil {
		return nil, fmt.Errorf("grouping packages: %w", err)
	}
	log.Infof("Building %d layers with budget %d", len(groups), bc.ic.Layering.Budget)

	for i, g := range groups {
		log.Infof("  layer[%d]:", i)

		for _, pkg := range g.pkgs {
			log.Infof("    - %s=%s", pkg.Name, pkg.Version)
		}
	}

	// Then partition that single fs.FS into multiple layers based on our layering strategy.
	layers, err := splitLayers(ctx, bc.fs, groups, pkgToDiff, bc.o.TempDir())
	if err != nil {
		return nil, err
	}

	// Push newly built layers to cache if configured
	if bc.layerCacheConfig != nil && bc.layerCacheConfig.Registry != "" {
		predictedGroups, err := bc.PredictLayerGroups(ctx)
		if err != nil {
			log.Warnf("failed to get layer groups for cache push: %v", err)
		} else if len(layers) == len(predictedGroups) {
			cache := newLayerCache(bc.layerCacheConfig, string(bc.o.Arch))
			if err := cache.pushLayers(ctx, layers, predictedGroups); err != nil {
				log.Warnf("failed to push layers to cache: %v", err)
			}
		}
	}

	return layers, nil
}

// tryLayerCache checks if all layers exist in cache and returns them if so.
// Returns (layers, true, nil) on full cache hit, (nil, false, nil) on miss,
// or (nil, false, err) on error.
func (bc *Context) tryLayerCache(ctx context.Context) ([]v1.Layer, bool, error) {
	log := clog.FromContext(ctx)

	// Predict what layers would be built
	predictedGroups, err := bc.PredictLayerGroups(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("predicting layer groups: %w", err)
	}
	log.Infof("predicted %d layer groups", len(predictedGroups))

	// Check which layers are cached
	cache := newLayerCache(bc.layerCacheConfig, string(bc.o.Arch))
	cachedRefs, allCached, err := cache.checkLayers(ctx, predictedGroups)
	if err != nil {
		return nil, false, fmt.Errorf("checking layer cache: %w", err)
	}

	if !allCached {
		log.Infof("partial cache hit: %d/%d layers cached, building all", len(cachedRefs), len(predictedGroups))
		return nil, false, nil
	}

	// All layers cached - pull and return
	log.Infof("all %d layers cached, skipping build", len(predictedGroups))
	layers, err := cache.pullLayers(ctx, cachedRefs)
	if err != nil {
		return nil, false, fmt.Errorf("pulling cached layers: %w", err)
	}

	return layers, true, nil
}

func replacesGroup(rep string, g *group) (bool, error) {
	constraint := apk.ResolvePackageNameVersionPin(rep)

	// Look for the package to make sure the version satisfies Replaces.
	for _, pkg := range g.pkgs {
		if pkg.Name != constraint.Name {
			// This is not the package we're looking for.
			continue
		}

		ver, err := apk.ParseVersion(pkg.Version)
		if err != nil {
			return false, fmt.Errorf("parsing %s version %s: %w", pkg.Name, pkg.Version, err)
		}

		ok, err := constraint.SatisfiedBy(ver)
		if err != nil {
			return false, fmt.Errorf("checking %s satisfies %s: %w", pkg.Version, constraint.Name, err)
		}

		if ok {
			return true, nil
		}
	}

	return false, nil
}

func groupByOriginAndSize(pkgs []*apk.Package, budget int) ([]*group, error) {
	// First, we're going to group packages by their origin.
	byOrigin := map[string]*group{}
	for _, pkg := range pkgs {
		origin := pkg.Origin
		if _, ok := byOrigin[origin]; !ok {
			byOrigin[origin] = &group{}
		}

		g, ok := byOrigin[origin]
		if !ok {
			panic(fmt.Errorf("byOrigin[%q] missing", origin))
		}

		g.pkgs = append(g.pkgs, pkg)
	}

	// Then we need to merge any packages that replace each other.
	byPackage := map[string]*group{}
	for _, g := range byOrigin {
		for _, pkg := range g.pkgs {
			byPackage[pkg.Name] = g
		}
	}

	replaceMap := map[string][]string{}
	for _, g := range byPackage {
		for _, pkg := range g.pkgs {
			if len(pkg.Replaces) == 0 {
				continue
			}

			replaceMap[pkg.Name] = pkg.Replaces
		}
	}

	for pkg, replaces := range replaceMap {
		for _, rep := range replaces {
			constraint := apk.ResolvePackageNameVersionPin(rep)

			replacee, ok := byPackage[constraint.Name]
			if !ok {
				// Whatever this package replaces is not in the image, that's normal.
				continue
			}

			if ok, err := replacesGroup(rep, replacee); err != nil {
				return nil, fmt.Errorf("checking %s replaces %s: %w", pkg, constraint.Name, err)
			} else if !ok {
				continue
			}

			g, ok := byPackage[pkg]
			if !ok {
				panic(fmt.Errorf("byPackage[%q] missing", pkg))
			}

			// If they're already merged, nothing to do.
			if replacee == g {
				continue
			}

			// Otherwise, we need to merge the two groups.
			merged := merge(g, replacee)

			// Update our maps so we can test identity above.
			for _, pkg := range merged.pkgs {
				byPackage[pkg.Name] = merged
				byOrigin[pkg.Origin] = merged
			}
		}
	}

	// Now we need to pick the best groups to keep.
	// First pass we'll set the size of each group to the sum of the installed size of all its packages.
	groups := make([]*group, 0, budget)
	seen := map[*group]struct{}{}
	for v := range maps.Values(byOrigin) {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		groups = append(groups, v)
	}
	for _, g := range groups {
		for _, pkg := range g.pkgs {
			g.size += pkg.InstalledSize
			g.tiebreaker = max(g.tiebreaker, pkg.Name)
		}
	}

	// Then we'll sort by the size and take the top $budget, merging the remainders.
	slices.SortFunc(groups, func(a, b *group) int {
		return cmp.Or(
			cmp.Compare(b.size, a.size),             // Descending size.
			cmp.Compare(a.tiebreaker, b.tiebreaker)) // In the rare case where we have identical sizes.
	})

	if len(groups) > budget {
		cutoff := max(budget-1, 0) // Even if budget == 0, we want 1 group.

		remainder := groups[cutoff:]
		groups = groups[:cutoff]

		groups = append(groups, merge(remainder...))
	}

	// Sort packages too just so they're in a consistent order.
	for _, g := range groups {
		slices.SortFunc(g.pkgs, func(a, b *apk.Package) int {
			return cmp.Compare(a.Name, b.Name)
		})
	}

	return groups, nil
}

type group struct {
	pkgs []*apk.Package

	size uint64

	// This is silly but in the event that two groups have identical size,
	// we want a predictable sort order _somehow_.
	tiebreaker string
}

// LayerGroup represents a group of packages that will be placed in the same layer.
// This is used for cache key computation before building.
type LayerGroup struct {
	// Packages is the list of packages in this layer, sorted by name.
	Packages []PackageRef

	// AllPackages is set only for the "top" layer (which has Packages=nil).
	// It contains all packages in the entire build, used to ensure the top
	// layer's cache key is unique per package set.
	AllPackages []PackageRef
}

// PackageRef identifies a package by name and version for cache key purposes.
type PackageRef struct {
	Name    string
	Version string
}

// GroupByOriginAndSize groups packages by their origin and size for layering.
// This is the same algorithm used internally by BuildLayers.
// The budget parameter controls the maximum number of layers (0 means unlimited).
func GroupByOriginAndSize(pkgs []*apk.Package, budget int) ([]LayerGroup, error) {
	groups, err := groupByOriginAndSize(pkgs, budget)
	if err != nil {
		return nil, err
	}

	// Build a sorted list of all packages for the top layer's cache key
	allPkgRefs := make([]PackageRef, len(pkgs))
	for i, pkg := range pkgs {
		allPkgRefs[i] = PackageRef{
			Name:    pkg.Name,
			Version: pkg.Version,
		}
	}
	slices.SortFunc(allPkgRefs, func(a, b PackageRef) int {
		return cmp.Compare(a.Name, b.Name)
	})

	result := make([]LayerGroup, len(groups))
	for i, g := range groups {
		refs := make([]PackageRef, len(g.pkgs))
		for j, pkg := range g.pkgs {
			refs[j] = PackageRef{
				Name:    pkg.Name,
				Version: pkg.Version,
			}
		}
		result[i] = LayerGroup{Packages: refs}
	}

	// Note: There's also a "top" layer for files not owned by any package.
	// We set AllPackages so the cache key is unique per package set.
	result = append(result, LayerGroup{Packages: nil, AllPackages: allPkgRefs})

	return result, nil
}

func merge(groups ...*group) *group {
	merged := &group{}
	for _, g := range groups {
		merged.pkgs = slices.Concat(merged.pkgs, g.pkgs)
		merged.size += g.size
		merged.tiebreaker = max(merged.tiebreaker, g.tiebreaker)
	}
	return merged
}

func splitLayers(ctx context.Context, fsys apkfs.FullFS, groups []*group, pkgToDiff map[*apk.Package][]byte, tmpdir string) ([]v1.Layer, error) {
	buf := make([]byte, 1<<20)

	// We'll create a writer for each layer and a map to quickly access the writer given a package or group.
	packageToWriter := map[string]*layerWriter{}
	groupToWriter := map[*group]*layerWriter{}

	for _, g := range groups {
		f, err := os.CreateTemp(tmpdir, "layer-*.tar.gz")
		if err != nil {
			return nil, err
		}
		defer f.Close()

		w := newLayerWriter(f)
		groupToWriter[g] = w

		for _, pkg := range g.pkgs {
			packageToWriter[pkg.Name] = w
		}
	}

	// The top layer holds anything that doesn't belong to a package.
	f, err := os.CreateTemp(tmpdir, "layer-*.tar.gz")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	top := newLayerWriter(f)

	// In a tar file, it is customary to include directories before files in those directories.
	// In order to know which directories we need to include, we maintain a directory stack for each layer.
	// We compare those stacks to the full FS that we're walking whenever we create a new tar entry.
	// If the main stack doesn't match the layer's stack, we update the layer's stack and add
	// any missing directory entries to the layer before we write the actual file entry.
	stack := []*file{}

	for f, err := range walkFS(ctx, fsys) {
		if err != nil {
			return nil, err
		}

		// Maintain our "main" stack.
		if f.header.Typeflag == tar.TypeDir {
			// Pop off any directories that are not parents of the current file's directory.
			for i := len(stack) - 1; i >= 0; i-- {
				if stack[i].path == path.Dir(f.path) {
					break
				}

				stack = stack[:i]
			}

			// Push the current file onto the stack.
			stack = append(stack, f)
		}

		// By default, all files go into the top layer.
		w := top

		// However, if a file implements an extension interface that tells us what package owns it,
		// we can use that to determine which layer it belongs to (if any).
		if pkger, ok := f.info.(interface {
			Package() *apk.Package
		}); ok {
			if pkg := pkger.Package(); pkg != nil {
				w, ok = packageToWriter[pkg.Name]
				if !ok {
					panic(fmt.Errorf("packageToWriter[%q] missing", pkg.Name))
				}
			}
		}

		// As described above, bring the layer's stack up to date with the main stack.
		for _, todo := range w.alignStacks(stack) {
			// We need to write any missing directories returned by alignStacks.
			// But sometimes the result of alignStacks will include the file we're
			// about to write (f) after this loop. In those cases, make sure we
			// don't write it twice by skipping over it here.
			if todo.header == f.header {
				continue
			}

			// This is a little weird, but bear with me...
			// Often, multiple packages (and thus layers) contain files in the same directory.
			// The directories in each package likely have different timestamps.
			// The overall image's filesystem will only have one directory entry, so there
			// will be a "winner" timestamp that actually ends up in the image.
			// Unfortunately, this means that we don't get great layer deduplication
			// in a lot of situations, and the timestamp of some directories in any given
			// layer are influenced by the timestamp of directories in _other_ layers.
			// Since timestamps almost never have an observable effect on the image behavior,
			// and the "real" timestamp will end up in the "top" layer anyway and overwrite
			// the directory metadata for these package-ful layers, we can improve deduplication
			// without having any real effect on the image by overwriting this directory's
			// timestamp with the timestamp of the "f" file we're about to write to this layer.
			todo.header.ModTime = f.header.ModTime

			if err := w.w.WriteHeader(todo.header); err != nil {
				return nil, fmt.Errorf("writing header %s: %w", todo.header.Name, err)
			}
		}

		// Now we're back to normal tar stuff.
		if err := w.w.WriteHeader(f.header); err != nil {
			return nil, fmt.Errorf("writing header %s: %w", f.header.Name, err)
		}

		if f.header.Typeflag == tar.TypeReg && f.header.Size > 0 {
			data, err := fsys.Open(f.path)
			if err != nil {
				return nil, fmt.Errorf("opening %s: %w", f.path, err)
			}
			if _, err := io.CopyBuffer(w.w, data, buf); err != nil {
				return nil, fmt.Errorf("copying %s: %w", f.path, err)
			}

			// Should never fail in practice.
			if err := data.Close(); err != nil {
				return nil, fmt.Errorf("closing %s: %w", f.path, err)
			}
		}

		if f.header.Name == "usr/lib/apk/db/installed" {
			// Add a partial installed db to each layer to satisfy scanners.
			for _, g := range groups {
				w := groupToWriter[g]

				// Make sure we have all parent directories to appease AWS Lambda.
				for _, todo := range w.alignStacks(stack) {
					todo.header.ModTime = f.header.ModTime

					if err := w.w.WriteHeader(todo.header); err != nil {
						return nil, fmt.Errorf("writing header %s: %w", todo.header.Name, err)
					}
				}

				// Accumulate all the idb entries for this layer.
				var buf bytes.Buffer
				for _, pkg := range g.pkgs {
					if _, err := buf.Write(pkgToDiff[pkg]); err != nil {
						return nil, err
					}
				}

				// Only the size should be different across layers.
				idb := *f.header
				idb.Size = int64(buf.Len())

				if err := w.w.WriteHeader(&idb); err != nil {
					return nil, err
				}

				if _, err := io.Copy(w.w, &buf); err != nil {
					return nil, err
				}
			}
		}
	}

	// Once we're done walking the FS, we need to finalize each layer...
	layers := make([]v1.Layer, 0, len(groups)+1)
	for i, g := range groups {
		w := groupToWriter[g]

		l, err := w.finalize()
		if err != nil {
			return nil, fmt.Errorf("finalizing group[%d] layer: %w", i, err)
		}
		layers = append(layers, l)
	}

	// ...including the top layer.
	topLayer, err := top.finalize()
	if err != nil {
		return nil, fmt.Errorf("finalizing top layer: %w", err)
	}

	layers = append(layers, topLayer)

	return layers, nil
}

// alignStacks ensures that w.stack is aligned with the passed in "main" stack
// by updating w.stack and returning any directories w hasn't already seen written.
// This relies on the fact that WalkDir iterates in lexicographic order, so we will
// only ever write a tar entry the first time we see a dir (for a given layer).
//
// Examples...
//
// stack:   [etc]
// w.stack: []
// return:  [etc]
//
// stack:   [usr, usr/lib]
// w.stack: [etc, etc/apk, etc/apk/keys]
// return:  [usr, usr/lib]
//
// stack:   [usr, usr/lib]
// w.stack: [usr]
// return:  [usr/lib]
//
// stack:   [usr, usr/lib]
// w.stack: [usr, usr/bin]
// return:  [usr/lib]

// stack:   [usr, usr/lib]
// w.stack: [usr, usr/lib, usr/lib/foo]
// return:  []
func (w *layerWriter) alignStacks(stack []*file) []*file {
	for i := 0; i < max(len(w.stack), len(stack)); i++ {
		// The layer's stack is taller than the main stack, truncate layer's stack.
		if i >= len(stack) {
			w.stack = w.stack[:i]
			return nil
		}

		// Otherwise skip over any entries that are the same.
		if i < len(w.stack) && w.stack[i] == stack[i] {
			continue
		}

		// For anything left that's not the same, we'll truncate w.stack,
		// then append the main stack and return the difference.
		w.stack = w.stack[:i]
		w.stack = append(w.stack, stack[i:]...)
		return w.stack[i:]
	}

	return nil
}
