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
	"sync"
	"sync/atomic"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"chainguard.dev/apko/pkg/build/types"
)

// ImageCache caches built images by configuration hash.
// It supports coalescing: multiple requests for the same config will wait
// for the first build to complete rather than building redundantly.
type ImageCache struct {
	mu       sync.RWMutex
	cache    map[string]*CachedImage         // configHash → result
	inflight map[string]*inflightBuild       // configHash → in-progress build
	stats    imageCacheStats
}

// CachedImage stores a cached image result.
type CachedImage struct {
	Image      v1.Image
	ConfigHash string
	Arch       types.Architecture
	CreatedAt  time.Time
}

// inflightBuild represents a build that is currently in progress.
type inflightBuild struct {
	done   chan struct{}
	result *CachedImage
	err    error
}

// imageCacheStats tracks cache performance.
type imageCacheStats struct {
	hits       atomic.Int64
	misses     atomic.Int64
	coalesced  atomic.Int64 // requests that waited for an in-flight build
}

// ImageCacheStats contains statistics about the image cache.
type ImageCacheStats struct {
	Hits      int64 // Cache hits
	Misses    int64 // Cache misses (new builds)
	Coalesced int64 // Requests that waited for in-flight builds
	Size      int   // Current number of cached images
}

// NewImageCache creates a new image cache.
func NewImageCache() *ImageCache {
	return &ImageCache{
		cache:    make(map[string]*CachedImage),
		inflight: make(map[string]*inflightBuild),
	}
}

// GetOrBuild returns a cached image if available, otherwise builds it using the provided function.
// If another goroutine is already building the same image (same configHash), this call will
// wait for that build to complete rather than starting a redundant build.
func (c *ImageCache) GetOrBuild(ctx context.Context, configHash string, buildFn func(context.Context) (v1.Image, error)) (*CachedImage, error) {
	// Fast path: check cache with read lock
	c.mu.RLock()
	if cached, ok := c.cache[configHash]; ok {
		c.mu.RUnlock()
		c.stats.hits.Add(1)
		return cached, nil
	}

	// Check if build is in-flight
	if inflight, ok := c.inflight[configHash]; ok {
		c.mu.RUnlock()
		c.stats.coalesced.Add(1)
		// Wait for the in-flight build to complete
		select {
		case <-inflight.done:
			return inflight.result, inflight.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c.mu.RUnlock()

	// Slow path: need to build
	c.mu.Lock()
	// Double-check after acquiring write lock
	if cached, ok := c.cache[configHash]; ok {
		c.mu.Unlock()
		c.stats.hits.Add(1)
		return cached, nil
	}

	// Check again if another goroutine started building while we waited for the lock
	if inflight, ok := c.inflight[configHash]; ok {
		c.mu.Unlock()
		c.stats.coalesced.Add(1)
		select {
		case <-inflight.done:
			return inflight.result, inflight.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// Start new build
	inflight := &inflightBuild{done: make(chan struct{})}
	c.inflight[configHash] = inflight
	c.mu.Unlock()

	c.stats.misses.Add(1)

	// Do the build (outside the lock)
	img, err := buildFn(ctx)

	// Store result
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.inflight, configHash)

	if err != nil {
		inflight.err = err
		close(inflight.done)
		return nil, err
	}

	result := &CachedImage{
		Image:      img,
		ConfigHash: configHash,
		CreatedAt:  time.Now(),
	}
	c.cache[configHash] = result
	inflight.result = result
	close(inflight.done)

	return result, nil
}

// Get returns a cached image if available, without building.
func (c *ImageCache) Get(configHash string) (*CachedImage, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cached, ok := c.cache[configHash]
	if ok {
		c.stats.hits.Add(1)
	}
	return cached, ok
}

// Put stores an image in the cache.
func (c *ImageCache) Put(configHash string, img v1.Image) *CachedImage {
	c.mu.Lock()
	defer c.mu.Unlock()

	result := &CachedImage{
		Image:      img,
		ConfigHash: configHash,
		CreatedAt:  time.Now(),
	}
	c.cache[configHash] = result
	return result
}

// Delete removes an image from the cache.
func (c *ImageCache) Delete(configHash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cache, configHash)
}

// Clear removes all images from the cache.
func (c *ImageCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache = make(map[string]*CachedImage)
}

// Stats returns cache statistics.
func (c *ImageCache) Stats() ImageCacheStats {
	c.mu.RLock()
	size := len(c.cache)
	c.mu.RUnlock()

	return ImageCacheStats{
		Hits:      c.stats.hits.Load(),
		Misses:    c.stats.misses.Load(),
		Coalesced: c.stats.coalesced.Load(),
		Size:      size,
	}
}

// ResetStats resets cache statistics.
func (c *ImageCache) ResetStats() {
	c.stats.hits.Store(0)
	c.stats.misses.Store(0)
	c.stats.coalesced.Store(0)
}

// Evict removes cached images older than the given duration.
func (c *ImageCache) Evict(olderThan time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	cutoff := time.Now().Add(-olderThan)
	evicted := 0
	for hash, cached := range c.cache {
		if cached.CreatedAt.Before(cutoff) {
			delete(c.cache, hash)
			evicted++
		}
	}
	return evicted
}

// Size returns the number of cached images.
func (c *ImageCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache)
}

// defaultImageCache is the global default image cache.
var defaultImageCache *ImageCache
var defaultImageCacheOnce sync.Once

// DefaultImageCache returns the global default image cache.
// The cache is created lazily on first access.
func DefaultImageCache() *ImageCache {
	defaultImageCacheOnce.Do(func() {
		defaultImageCache = NewImageCache()
	})
	return defaultImageCache
}

// GetImageCacheStats returns statistics from the default image cache.
func GetImageCacheStats() ImageCacheStats {
	return DefaultImageCache().Stats()
}

// ResetImageCacheStats resets statistics on the default image cache.
func ResetImageCacheStats() {
	DefaultImageCache().ResetStats()
}

// ClearImageCache clears the default image cache.
func ClearImageCache() {
	DefaultImageCache().Clear()
}
