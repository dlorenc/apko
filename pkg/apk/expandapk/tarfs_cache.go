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

package expandapk

import (
	"encoding/hex"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"chainguard.dev/apko/internal/tarfs"
)

// TarFSCache caches tarfs.FS instances by package hash.
// This allows multiple builds using the same package to share the same
// in-memory tarfs index, reducing memory usage and index computation time.
type TarFSCache struct {
	mu    sync.RWMutex
	cache map[string]*cachedTarFS
	stats tarfsCacheStats
}

// cachedTarFS holds a cached tarfs.FS and its metadata.
type cachedTarFS struct {
	FS        *tarfs.FS
	File      *os.File // Underlying file handle (must stay open)
	FilePath  string   // Path to the file (for reopening if needed)
	CreatedAt time.Time
	LastUsed  time.Time
}

// tarfsCacheStats tracks cache performance.
type tarfsCacheStats struct {
	hits   atomic.Int64
	misses atomic.Int64
}

// TarFSCacheStats contains statistics about the tarfs cache.
type TarFSCacheStats struct {
	Hits   int64 // Cache hits
	Misses int64 // Cache misses
	Size   int   // Current number of cached entries
}

// globalTarFSCache is the default global tarfs cache.
var globalTarFSCache *TarFSCache
var globalTarFSCacheOnce sync.Once

// GlobalTarFSCache returns the global tarfs cache singleton.
func GlobalTarFSCache() *TarFSCache {
	globalTarFSCacheOnce.Do(func() {
		globalTarFSCache = NewTarFSCache()
	})
	return globalTarFSCache
}

// NewTarFSCache creates a new tarfs cache.
func NewTarFSCache() *TarFSCache {
	return &TarFSCache{
		cache: make(map[string]*cachedTarFS),
	}
}

// GetOrCreate returns a cached tarfs.FS for the given package hash,
// or creates a new one using the provided function if not cached.
//
// The create function should return:
// - The tarfs.FS instance
// - The underlying file handle (will be kept open by the cache)
// - The file path (for potential reopening)
// - Any error
func (c *TarFSCache) GetOrCreate(packageHash []byte, create func() (*tarfs.FS, *os.File, string, error)) (*tarfs.FS, error) {
	key := hex.EncodeToString(packageHash)

	// Fast path: check cache with read lock
	c.mu.RLock()
	if cached, ok := c.cache[key]; ok {
		cached.LastUsed = time.Now()
		c.mu.RUnlock()
		c.stats.hits.Add(1)
		return cached.FS, nil
	}
	c.mu.RUnlock()

	// Slow path: need to create
	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check after acquiring write lock
	if cached, ok := c.cache[key]; ok {
		cached.LastUsed = time.Now()
		c.stats.hits.Add(1)
		return cached.FS, nil
	}

	c.stats.misses.Add(1)

	// Create new tarfs
	fs, file, filePath, err := create()
	if err != nil {
		return nil, err
	}

	// Cache it
	c.cache[key] = &cachedTarFS{
		FS:        fs,
		File:      file,
		FilePath:  filePath,
		CreatedAt: time.Now(),
		LastUsed:  time.Now(),
	}

	return fs, nil
}

// Get returns a cached tarfs.FS if available.
func (c *TarFSCache) Get(packageHash []byte) (*tarfs.FS, bool) {
	key := hex.EncodeToString(packageHash)

	c.mu.RLock()
	defer c.mu.RUnlock()

	if cached, ok := c.cache[key]; ok {
		cached.LastUsed = time.Now()
		c.stats.hits.Add(1)
		return cached.FS, true
	}
	return nil, false
}

// Put stores a tarfs.FS in the cache.
func (c *TarFSCache) Put(packageHash []byte, fs *tarfs.FS, file *os.File, filePath string) {
	key := hex.EncodeToString(packageHash)

	c.mu.Lock()
	defer c.mu.Unlock()

	// If there's an existing entry, close its file first
	if existing, ok := c.cache[key]; ok && existing.File != nil {
		existing.File.Close()
	}

	c.cache[key] = &cachedTarFS{
		FS:        fs,
		File:      file,
		FilePath:  filePath,
		CreatedAt: time.Now(),
		LastUsed:  time.Now(),
	}
}

// Delete removes an entry from the cache and closes the file.
func (c *TarFSCache) Delete(packageHash []byte) {
	key := hex.EncodeToString(packageHash)

	c.mu.Lock()
	defer c.mu.Unlock()

	if cached, ok := c.cache[key]; ok {
		if cached.File != nil {
			cached.File.Close()
		}
		delete(c.cache, key)
	}
}

// Clear removes all entries from the cache and closes all files.
func (c *TarFSCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, cached := range c.cache {
		if cached.File != nil {
			cached.File.Close()
		}
	}
	c.cache = make(map[string]*cachedTarFS)
}

// Evict removes entries that haven't been used for the given duration.
// Returns the number of entries evicted.
func (c *TarFSCache) Evict(unusedFor time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	cutoff := time.Now().Add(-unusedFor)
	evicted := 0

	for key, cached := range c.cache {
		if cached.LastUsed.Before(cutoff) {
			if cached.File != nil {
				cached.File.Close()
			}
			delete(c.cache, key)
			evicted++
		}
	}

	return evicted
}

// Stats returns cache statistics.
func (c *TarFSCache) Stats() TarFSCacheStats {
	c.mu.RLock()
	size := len(c.cache)
	c.mu.RUnlock()

	return TarFSCacheStats{
		Hits:   c.stats.hits.Load(),
		Misses: c.stats.misses.Load(),
		Size:   size,
	}
}

// ResetStats resets cache statistics.
func (c *TarFSCache) ResetStats() {
	c.stats.hits.Store(0)
	c.stats.misses.Store(0)
}

// Size returns the number of cached entries.
func (c *TarFSCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache)
}

// Close closes the cache and all file handles.
// After Close, the cache should not be used.
func (c *TarFSCache) Close() error {
	c.Clear()
	return nil
}

// Closer wraps an io.ReaderAt to also implement io.Closer.
// This is used when the tarfs doesn't need to close its underlying reader.
type nopCloser struct {
	io.ReaderAt
}

func (nopCloser) Close() error { return nil }

// GetTarFSCacheStats returns statistics from the global tarfs cache.
func GetTarFSCacheStats() TarFSCacheStats {
	return GlobalTarFSCache().Stats()
}

// ResetTarFSCacheStats resets statistics on the global tarfs cache.
func ResetTarFSCacheStats() {
	GlobalTarFSCache().ResetStats()
}

// ClearTarFSCache clears the global tarfs cache.
func ClearTarFSCache() {
	GlobalTarFSCache().Clear()
}
