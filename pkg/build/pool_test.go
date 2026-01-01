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

package build

import (
	"sync"
	"testing"

	"chainguard.dev/apko/pkg/options"
)

// TestBoundedPool_PublicAPI tests the public API re-exported from internal/pool.
// More comprehensive tests are in internal/pool/pool_test.go.
func TestBoundedPool_PublicAPI(t *testing.T) {
	pool := NewBoundedPool(3, func() any {
		return make([]byte, 1024)
	})

	// Get and put items
	items := make([]any, 3)
	for i := range items {
		items[i] = pool.Get()
	}
	for _, item := range items {
		pool.Put(item)
	}

	stats := pool.Stats()
	if stats.Count != 3 {
		t.Errorf("expected count 3, got %d", stats.Count)
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

func TestPoolRegistry(t *testing.T) {
	pool := NewBoundedPool(5, func() any { return 1 })
	RegisterPool("build-test-pool", pool)

	retrieved, ok := GetRegisteredPool("build-test-pool")
	if !ok {
		t.Error("expected to find build-test-pool")
	}
	if retrieved != pool {
		t.Error("retrieved wrong pool")
	}

	stats := AllPoolStats()
	if _, ok := stats["build-test-pool"]; !ok {
		t.Error("expected build-test-pool in stats")
	}
}

func TestPoolStats_HitRate(t *testing.T) {
	stats := PoolStats{
		Hits:   75,
		Misses: 25,
	}
	if got := stats.HitRate(); got != 75.0 {
		t.Errorf("HitRate() = %v, want 75.0", got)
	}
}

func TestClearPools(t *testing.T) {
	pool := NewBoundedPool(5, func() any {
		return make([]byte, 1024)
	})
	RegisterPool("build-clear-test", pool)

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

	ClearPools()

	stats = pool.Stats()
	if stats.Count != 0 {
		t.Errorf("expected count 0 after clear, got %d", stats.Count)
	}
}

func TestConfigurePoolsFromOptions(t *testing.T) {
	// Create test pools with initial size 0 (uncapped)
	testPool1 := NewBoundedPool(0, func() any { return 1 })
	testPool2 := NewBoundedPool(0, func() any { return 2 })
	RegisterPool("expandapk-test-config", testPool1)
	RegisterPool("tarfs-reader", testPool2)

	// Verify initial state is uncapped
	stats1 := testPool1.Stats()
	stats2 := testPool2.Stats()
	if stats1.MaxSize != 0 {
		t.Errorf("expected initial maxSize 0, got %d", stats1.MaxSize)
	}
	if stats2.MaxSize != 0 {
		t.Errorf("expected initial maxSize 0, got %d", stats2.MaxSize)
	}

	// Configure pools
	opts := &options.Options{
		ExpandAPKPoolSize: 15,
		TarFSPoolSize:     25,
	}
	ConfigurePoolsFromOptions(opts)

	// Verify pools were configured
	stats1 = testPool1.Stats()
	stats2 = testPool2.Stats()
	if stats1.MaxSize != 15 {
		t.Errorf("expected maxSize 15, got %d", stats1.MaxSize)
	}
	if stats2.MaxSize != 25 {
		t.Errorf("expected maxSize 25, got %d", stats2.MaxSize)
	}
}

func TestConfigurePoolsForService(t *testing.T) {
	// Create a test pool
	testPool := NewBoundedPool(0, func() any { return 1 })
	RegisterPool("expandapk-service-test", testPool)

	// Configure for service
	ConfigurePoolsForService()

	// Verify pool was configured with recommended size
	stats := testPool.Stats()
	if stats.MaxSize != int64(options.RecommendedExpandAPKPoolSize) {
		t.Errorf("expected maxSize %d, got %d", options.RecommendedExpandAPKPoolSize, stats.MaxSize)
	}
}

// Benchmarks comparing bounded pool vs standard sync.Pool

func BenchmarkBoundedPool_GetPut(b *testing.B) {
	pool := NewBoundedPool(100, func() any {
		return make([]byte, 1<<20)
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		item := pool.Get()
		pool.Put(item)
	}
}

func BenchmarkBoundedPool_GetPutParallel(b *testing.B) {
	pool := NewBoundedPool(100, func() any {
		return make([]byte, 1<<20)
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
			return make([]byte, 1<<20)
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
			return make([]byte, 1<<20)
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

// BenchmarkBoundedPool_SimulateConcurrentBuilds simulates apko-as-a-service
func BenchmarkBoundedPool_SimulateConcurrentBuilds(b *testing.B) {
	pool := NewBoundedPool(20, func() any {
		return make([]byte, 1<<20)
	})

	b.ResetTimer()
	b.SetParallelism(12)

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
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

func BenchmarkSyncPool_SimulateConcurrentBuilds(b *testing.B) {
	pool := sync.Pool{
		New: func() any {
			return make([]byte, 1<<20)
		},
	}

	b.ResetTimer()
	b.SetParallelism(12)

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			for i := 0; i < 10; i++ {
				item := pool.Get()
				pool.Put(item)
			}
		}
	})
}
