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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"sync"

	"github.com/chainguard-dev/clog"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// LayerCacheConfig configures layer caching for BuildLayers.
type LayerCacheConfig struct {
	// Registry is the base registry URL for cached layers.
	// Example: "registry:5000/apko-layers"
	Registry string

	// Insecure allows connecting to registries over HTTP.
	Insecure bool
}

// layerCache implements layer caching operations.
type layerCache struct {
	config *LayerCacheConfig
	arch   string
}

// newLayerCache creates a new layer cache.
func newLayerCache(config *LayerCacheConfig, arch string) *layerCache {
	return &layerCache{
		config: config,
		arch:   arch,
	}
}

// cacheKey computes a deterministic cache key for a layer group.
// The key is based on the architecture and sorted package names+versions.
func (c *layerCache) cacheKey(group LayerGroup) string {
	h := sha256.New()

	// Include architecture
	h.Write([]byte(c.arch + "\n"))

	// Sort packages for determinism
	pkgs := make([]string, len(group.Packages))
	for i, p := range group.Packages {
		pkgs[i] = fmt.Sprintf("%s=%s", p.Name, p.Version)
	}
	slices.Sort(pkgs)

	// Hash the package list
	for _, pkg := range pkgs {
		h.Write([]byte(pkg + "\n"))
	}

	return hex.EncodeToString(h.Sum(nil))[:16]
}

// cachedLayerRef represents a reference to a cached layer.
type cachedLayerRef struct {
	key string
	ref string
}

// checkLayers checks which layer groups exist in the registry.
// Returns refs for layers that exist and whether all were found.
func (c *layerCache) checkLayers(ctx context.Context, groups []LayerGroup) ([]cachedLayerRef, bool, error) {
	log := clog.FromContext(ctx)

	if c.config.Registry == "" {
		return nil, false, nil
	}

	// Compute cache keys for all groups
	keys := make([]string, len(groups))
	for i, g := range groups {
		keys[i] = c.cacheKey(g)
	}

	// Check all layers in parallel
	var wg sync.WaitGroup
	var mu sync.Mutex
	found := make([]cachedLayerRef, 0, len(groups))
	errs := make([]error, 0)

	opts := []name.Option{}
	if c.config.Insecure {
		opts = append(opts, name.Insecure)
	}
	remoteOpts := []remote.Option{remote.WithContext(ctx)}
	if c.config.Insecure {
		remoteOpts = append(remoteOpts, remote.WithTransport(&http.Transport{}))
	}

	for _, key := range keys {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()

			ref := fmt.Sprintf("%s:%s", c.config.Registry, k)
			imgRef, err := name.ParseReference(ref, opts...)
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("parsing ref %s: %w", ref, err))
				mu.Unlock()
				return
			}

			if _, err := remote.Head(imgRef, remoteOpts...); err == nil {
				mu.Lock()
				found = append(found, cachedLayerRef{key: k, ref: ref})
				mu.Unlock()
			}
		}(key)
	}
	wg.Wait()

	if len(errs) > 0 {
		log.Warnf("errors checking layer cache: %v", errs)
	}

	allCached := len(found) == len(groups)
	log.Infof("layer cache: %d/%d layers found", len(found), len(groups))

	return found, allCached, nil
}

// pullLayers retrieves cached layers from the registry.
func (c *layerCache) pullLayers(ctx context.Context, refs []cachedLayerRef) ([]v1.Layer, error) {
	log := clog.FromContext(ctx)

	opts := []name.Option{}
	if c.config.Insecure {
		opts = append(opts, name.Insecure)
	}
	remoteOpts := []remote.Option{remote.WithContext(ctx)}
	if c.config.Insecure {
		remoteOpts = append(remoteOpts, remote.WithTransport(&http.Transport{}))
	}

	layers := make([]v1.Layer, len(refs))
	for i, ref := range refs {
		imgRef, err := name.ParseReference(ref.ref, opts...)
		if err != nil {
			return nil, fmt.Errorf("parsing ref %s: %w", ref.ref, err)
		}

		img, err := remote.Image(imgRef, remoteOpts...)
		if err != nil {
			return nil, fmt.Errorf("pulling layer %s: %w", ref.ref, err)
		}

		imgLayers, err := img.Layers()
		if err != nil {
			return nil, fmt.Errorf("getting layers from %s: %w", ref.ref, err)
		}

		if len(imgLayers) != 1 {
			return nil, fmt.Errorf("expected 1 layer in %s, got %d", ref.ref, len(imgLayers))
		}

		layers[i] = imgLayers[0]
		log.Debugf("pulled cached layer %s", ref.key)
	}

	log.Infof("pulled %d layers from cache", len(layers))
	return layers, nil
}

// pushLayers pushes layers to the registry cache.
func (c *layerCache) pushLayers(ctx context.Context, layers []v1.Layer, groups []LayerGroup) error {
	log := clog.FromContext(ctx)

	if c.config.Registry == "" {
		return nil
	}

	if len(layers) != len(groups) {
		return fmt.Errorf("layer count mismatch: %d layers vs %d groups", len(layers), len(groups))
	}

	opts := []name.Option{}
	if c.config.Insecure {
		opts = append(opts, name.Insecure)
	}
	remoteOpts := []remote.Option{remote.WithContext(ctx)}
	if c.config.Insecure {
		remoteOpts = append(remoteOpts, remote.WithTransport(&http.Transport{}))
	}

	pushed := 0
	skipped := 0
	for i, layer := range layers {
		key := c.cacheKey(groups[i])
		ref := fmt.Sprintf("%s:%s", c.config.Registry, key)

		imgRef, err := name.ParseReference(ref, opts...)
		if err != nil {
			return fmt.Errorf("parsing ref %s: %w", ref, err)
		}

		// Check if already exists
		if _, err := remote.Head(imgRef, remoteOpts...); err == nil {
			log.Debugf("layer %s already cached", key)
			skipped++
			continue
		}

		// Wrap layer in a minimal image for storage
		img, err := mutate.AppendLayers(empty.Image, layer)
		if err != nil {
			return fmt.Errorf("wrapping layer %d: %w", i, err)
		}

		if err := remote.Write(imgRef, img, remoteOpts...); err != nil {
			return fmt.Errorf("pushing layer %s: %w", ref, err)
		}
		log.Debugf("pushed layer %s", key)
		pushed++
	}

	if pushed > 0 {
		log.Infof("pushed %d new layers to cache (%d already cached)", pushed, skipped)
	} else if skipped > 0 {
		log.Infof("all %d layers already cached", skipped)
	}
	return nil
}
