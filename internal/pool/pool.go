// Copyright 2022, 2023 Chainguard, Inc.
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

// Package pool provides bounded sync.Pool implementations for memory management
// in long-running services like apko-as-a-service.
package pool

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// BoundedPool is a sync.Pool with a configurable maximum size.
// Unlike sync.Pool which grows unboundedly in long-running processes,
// BoundedPool limits the number of items it retains.
//
// When Put is called and the pool is at capacity, the item is discarded
// (allowing it to be garbage collected) instead of being added to the pool.
//
// This is particularly useful for apko-as-a-service scenarios where many
// concurrent builds would otherwise cause sync.Pool to retain large amounts
// of memory indefinitely.
type BoundedPool struct {
	pool    sync.Pool
	count   atomic.Int64
	maxSize int64

	// Metrics for monitoring pool efficiency
	hits   atomic.Int64 // Items retrieved from pool
	misses atomic.Int64 // Items created via New
	drops  atomic.Int64 // Items dropped due to capacity
}

// NewBoundedPool creates a new bounded pool with the specified maximum size.
// The newFunc is called when Get needs to create a new item (same as sync.Pool.New).
// If maxSize is 0 or negative, no limit is enforced (behaves like sync.Pool).
func NewBoundedPool(maxSize int, newFunc func() any) *BoundedPool {
	return &BoundedPool{
		pool: sync.Pool{
			New: newFunc,
		},
		maxSize: int64(maxSize),
	}
}

// Get retrieves an item from the pool.
// If the pool is empty, it creates a new item using the New function.
// Note: We can't distinguish between items from the pool vs newly created,
// so hits/misses are tracked via Put (drops indicate capacity limits hit).
func (p *BoundedPool) Get() any {
	// Decrement count optimistically; if item was from New, count may go negative
	// but will be corrected on Put. This is acceptable for monitoring purposes.
	current := p.count.Add(-1)
	if current < 0 {
		// Item was newly created, not from pool
		p.count.Add(1) // Correct the count
		p.misses.Add(1)
	} else {
		p.hits.Add(1)
	}
	return p.pool.Get()
}

// Put returns an item to the pool.
// If the pool is at capacity, the item is discarded.
func (p *BoundedPool) Put(x any) {
	if x == nil {
		return
	}

	// If no limit, always put
	if p.maxSize <= 0 {
		p.count.Add(1)
		p.pool.Put(x)
		return
	}

	// Only put if we're under capacity
	for {
		current := p.count.Load()
		if current >= p.maxSize {
			// At capacity, let it be GC'd
			p.drops.Add(1)
			return
		}
		if p.count.CompareAndSwap(current, current+1) {
			p.pool.Put(x)
			return
		}
	}
}

// Stats returns the current pool statistics.
func (p *BoundedPool) Stats() Stats {
	return Stats{
		Count:   p.count.Load(),
		MaxSize: p.maxSize,
		Hits:    p.hits.Load(),
		Misses:  p.misses.Load(),
		Drops:   p.drops.Load(),
	}
}

// SetMaxSize updates the maximum pool size.
// If maxSize is 0 or negative, the pool becomes uncapped.
// This can be called at any time and takes effect immediately for subsequent Put operations.
func (p *BoundedPool) SetMaxSize(maxSize int) {
	p.maxSize = int64(maxSize)
}

// ResetMetrics resets the hit/miss/drop counters.
func (p *BoundedPool) ResetMetrics() {
	p.hits.Store(0)
	p.misses.Store(0)
	p.drops.Store(0)
}

// ResetCount resets the count to 0, typically after GC has cleared the underlying pool.
func (p *BoundedPool) ResetCount() {
	p.count.Store(0)
}

// Stats contains statistics about a BoundedPool.
type Stats struct {
	Count   int64 // Current number of items in pool
	MaxSize int64 // Maximum pool size
	Hits    int64 // Number of successful Gets from pool
	Misses  int64 // Number of Gets that required New
	Drops   int64 // Number of Puts that were dropped due to capacity
}

// HitRate returns the cache hit rate as a percentage (0-100).
func (s Stats) HitRate() float64 {
	total := s.Hits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(total) * 100
}

// Registry maintains a registry of all bounded pools for centralized management.
// This is useful for long-running services that need to clear pools periodically.
type Registry struct {
	mu    sync.RWMutex
	pools map[string]*BoundedPool
}

// GlobalRegistry is the default pool registry.
var GlobalRegistry = &Registry{
	pools: make(map[string]*BoundedPool),
}

// Register registers a named pool with the registry.
func (r *Registry) Register(name string, pool *BoundedPool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pools[name] = pool
}

// Get retrieves a named pool from the registry.
func (r *Registry) Get(name string) (*BoundedPool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	pool, ok := r.pools[name]
	return pool, ok
}

// AllStats returns statistics for all registered pools.
func (r *Registry) AllStats() map[string]Stats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	stats := make(map[string]Stats, len(r.pools))
	for name, pool := range r.pools {
		stats[name] = pool.Stats()
	}
	return stats
}

// Clear triggers garbage collection to clear sync.Pool contents
// and resets pool counters. This is useful between builds in long-running
// services to prevent unbounded memory growth.
//
// Note: sync.Pool automatically clears items during GC, but this function
// forces an immediate GC cycle and resets the bounded pool counters.
func (r *Registry) Clear() {
	// Force GC to clear sync.Pool contents
	// sync.Pool items are cleared during GC
	runtime.GC()

	// Reset counters for all registered pools
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, pool := range r.pools {
		pool.ResetCount()
	}
}

// ResetAllMetrics resets the hit/miss/drop counters for all registered pools.
// This is useful for gathering metrics over specific time periods.
func (r *Registry) ResetAllMetrics() {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, pool := range r.pools {
		pool.ResetMetrics()
	}
}

// Convenience functions using the global registry

// Register registers a named pool with the global registry.
func Register(name string, p *BoundedPool) {
	GlobalRegistry.Register(name, p)
}

// GetPool retrieves a named pool from the global registry.
func GetPool(name string) (*BoundedPool, bool) {
	return GlobalRegistry.Get(name)
}

// AllStats returns statistics for all registered pools in the global registry.
func AllStats() map[string]Stats {
	return GlobalRegistry.AllStats()
}

// ClearAll triggers garbage collection and resets all pools in the global registry.
func ClearAll() {
	GlobalRegistry.Clear()
}

// ResetAllMetrics resets metrics for all pools in the global registry.
func ResetAllMetrics() {
	GlobalRegistry.ResetAllMetrics()
}
