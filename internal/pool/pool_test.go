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

package pool

import (
	"sync"
	"testing"
)

func TestBoundedPool_Basic(t *testing.T) {
	createCount := 0
	pool := NewBoundedPool(3, func() any {
		createCount++
		return make([]byte, 1024)
	})

	// Get 3 items - should create them
	items := make([]any, 3)
	for i := 0; i < 3; i++ {
		items[i] = pool.Get()
	}

	if createCount != 3 {
		t.Errorf("expected 3 creates, got %d", createCount)
	}

	// Put them all back
	for _, item := range items {
		pool.Put(item)
	}

	stats := pool.Stats()
	if stats.Count != 3 {
		t.Errorf("expected count 3, got %d", stats.Count)
	}

	// Get them again - should reuse
	for i := 0; i < 3; i++ {
		items[i] = pool.Get()
	}

	if createCount != 3 {
		t.Errorf("expected still 3 creates (reuse), got %d", createCount)
	}

	stats = pool.Stats()
	if stats.Hits != 3 {
		t.Errorf("expected 3 hits, got %d", stats.Hits)
	}
}

func TestBoundedPool_ExceedsCapacity(t *testing.T) {
	pool := NewBoundedPool(2, func() any {
		return make([]byte, 1024)
	})

	// Get and put 4 items - only 2 should be retained
	items := make([]any, 4)
	for i := 0; i < 4; i++ {
		items[i] = pool.Get()
	}

	for _, item := range items {
		pool.Put(item)
	}

	stats := pool.Stats()
	if stats.Count != 2 {
		t.Errorf("expected count 2 (capped), got %d", stats.Count)
	}
	if stats.Drops != 2 {
		t.Errorf("expected 2 drops, got %d", stats.Drops)
	}
}

func TestBoundedPool_NoLimit(t *testing.T) {
	pool := NewBoundedPool(0, func() any {
		return make([]byte, 1024)
	})

	// Get and put 10 items - all should be retained when maxSize is 0
	items := make([]any, 10)
	for i := 0; i < 10; i++ {
		items[i] = pool.Get()
	}

	for _, item := range items {
		pool.Put(item)
	}

	stats := pool.Stats()
	if stats.Drops != 0 {
		t.Errorf("expected 0 drops with no limit, got %d", stats.Drops)
	}
}

func TestBoundedPool_NilPut(t *testing.T) {
	pool := NewBoundedPool(3, func() any {
		return make([]byte, 1024)
	})

	// Put nil should be a no-op
	pool.Put(nil)

	stats := pool.Stats()
	if stats.Count != 0 {
		t.Errorf("expected count 0 after nil put, got %d", stats.Count)
	}
}

func TestBoundedPool_Concurrent(t *testing.T) {
	pool := NewBoundedPool(10, func() any {
		return make([]byte, 1024)
	})

	var wg sync.WaitGroup
	const goroutines = 100
	const iterations = 100

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				item := pool.Get()
				// Simulate some work
				pool.Put(item)
			}
		}()
	}

	wg.Wait()

	stats := pool.Stats()
	// Count should never exceed maxSize
	if stats.Count > 10 {
		t.Errorf("count %d exceeded maxSize 10", stats.Count)
	}

	// Total gets should equal hits + misses
	totalGets := stats.Hits + stats.Misses
	expectedGets := int64(goroutines * iterations)
	if totalGets != expectedGets {
		t.Errorf("expected %d total gets, got %d", expectedGets, totalGets)
	}
}

func TestStats_HitRate(t *testing.T) {
	tests := []struct {
		name     string
		hits     int64
		misses   int64
		expected float64
	}{
		{"all hits", 100, 0, 100.0},
		{"all misses", 0, 100, 0.0},
		{"50-50", 50, 50, 50.0},
		{"no data", 0, 0, 0.0},
		{"75%", 75, 25, 75.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stats := Stats{
				Hits:   tt.hits,
				Misses: tt.misses,
			}
			if got := stats.HitRate(); got != tt.expected {
				t.Errorf("HitRate() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestRegistry(t *testing.T) {
	// Create a new registry for isolated testing
	reg := &Registry{pools: make(map[string]*BoundedPool)}

	// Create and register pools
	pool1 := NewBoundedPool(5, func() any { return 1 })
	pool2 := NewBoundedPool(10, func() any { return 2 })

	reg.Register("test-pool-1", pool1)
	reg.Register("test-pool-2", pool2)

	// Verify retrieval
	retrieved, ok := reg.Get("test-pool-1")
	if !ok {
		t.Error("expected to find test-pool-1")
	}
	if retrieved != pool1 {
		t.Error("retrieved wrong pool")
	}

	_, ok = reg.Get("nonexistent")
	if ok {
		t.Error("expected to not find nonexistent pool")
	}

	// Use the pools
	items := make([]any, 3)
	for i := range items {
		items[i] = pool1.Get()
	}
	for _, item := range items {
		pool1.Put(item)
	}

	// Get all stats
	allStats := reg.AllStats()
	if _, ok := allStats["test-pool-1"]; !ok {
		t.Error("expected test-pool-1 in stats")
	}
	if _, ok := allStats["test-pool-2"]; !ok {
		t.Error("expected test-pool-2 in stats")
	}
}

func TestResetMetrics(t *testing.T) {
	pool := NewBoundedPool(5, func() any { return 1 })

	// Generate some activity
	for i := 0; i < 10; i++ {
		item := pool.Get()
		pool.Put(item)
	}

	stats := pool.Stats()
	if stats.Hits+stats.Misses == 0 {
		t.Error("expected some hits or misses")
	}

	// Reset metrics
	pool.ResetMetrics()

	stats = pool.Stats()
	if stats.Hits != 0 || stats.Misses != 0 || stats.Drops != 0 {
		t.Errorf("expected all metrics reset, got hits=%d, misses=%d, drops=%d",
			stats.Hits, stats.Misses, stats.Drops)
	}
}

func TestClear(t *testing.T) {
	reg := &Registry{pools: make(map[string]*BoundedPool)}

	pool := NewBoundedPool(5, func() any {
		return make([]byte, 1024)
	})
	reg.Register("test-clear-pool", pool)

	// Fill the pool
	items := make([]any, 5)
	for i := range items {
		items[i] = pool.Get()
	}
	for _, item := range items {
		pool.Put(item)
	}

	stats := pool.Stats()
	if stats.Count != 5 {
		t.Errorf("expected count 5 before clear, got %d", stats.Count)
	}

	// Clear pools (this forces GC)
	reg.Clear()

	// After Clear, the count should be reset
	stats = pool.Stats()
	if stats.Count != 0 {
		t.Errorf("expected count 0 after clear, got %d", stats.Count)
	}
}

// Benchmarks

func BenchmarkBoundedPool_GetPut(b *testing.B) {
	pool := NewBoundedPool(100, func() any {
		return make([]byte, 1<<20) // 1MB
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		item := pool.Get()
		pool.Put(item)
	}
}

func BenchmarkBoundedPool_GetPutParallel(b *testing.B) {
	pool := NewBoundedPool(100, func() any {
		return make([]byte, 1<<20) // 1MB
	})

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			item := pool.Get()
			pool.Put(item)
		}
	})
}

func BenchmarkSyncPool_GetPut(b *testing.B) {
	pool := sync.Pool{
		New: func() any {
			return make([]byte, 1<<20) // 1MB
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		item := pool.Get()
		pool.Put(item)
	}
}

func BenchmarkSyncPool_GetPutParallel(b *testing.B) {
	pool := sync.Pool{
		New: func() any {
			return make([]byte, 1<<20) // 1MB
		},
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			item := pool.Get()
			pool.Put(item)
		}
	})
}

// BenchmarkBoundedPool_AtCapacity tests performance when pool is at capacity
func BenchmarkBoundedPool_AtCapacity(b *testing.B) {
	pool := NewBoundedPool(10, func() any {
		return make([]byte, 1<<20) // 1MB
	})

	// Fill to capacity
	items := make([]any, 10)
	for i := range items {
		items[i] = pool.Get()
	}
	for _, item := range items {
		pool.Put(item)
	}

	// Now benchmark puts that will be dropped
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		item := pool.Get()
		pool.Put(item)
	}
}

// BenchmarkBoundedPool_SimulateConcurrentBuilds simulates the apko-as-a-service scenario
func BenchmarkBoundedPool_SimulateConcurrentBuilds(b *testing.B) {
	// Simulate 12 concurrent builds with bounded pool (max 20 items)
	pool := NewBoundedPool(20, func() any {
		return make([]byte, 1<<20) // 1MB buffers
	})

	b.ResetTimer()
	b.SetParallelism(12) // Simulate 12 concurrent builds

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Each "build" does multiple get/put cycles
			for i := 0; i < 10; i++ {
				item := pool.Get()
				pool.Put(item)
			}
		}
	})

	b.StopTimer()
	stats := pool.Stats()
	b.Logf("Pool stats - Count: %d, Hits: %d, Misses: %d, Drops: %d, HitRate: %.1f%%",
		stats.Count, stats.Hits, stats.Misses, stats.Drops, stats.HitRate())
}

// BenchmarkSyncPool_SimulateConcurrentBuilds for comparison
func BenchmarkSyncPool_SimulateConcurrentBuilds(b *testing.B) {
	// Standard sync.Pool (unbounded)
	pool := sync.Pool{
		New: func() any {
			return make([]byte, 1<<20) // 1MB buffers
		},
	}

	b.ResetTimer()
	b.SetParallelism(12) // Simulate 12 concurrent builds

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Each "build" does multiple get/put cycles
			for i := 0; i < 10; i++ {
				item := pool.Get()
				pool.Put(item)
			}
		}
	})
}
