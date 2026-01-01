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

// This file re-exports pool types from internal/pool for external use.
// The core implementation lives in internal/pool, while this file provides
// the public API and build-specific convenience functions.

package build

import (
	"strings"

	"chainguard.dev/apko/internal/pool"
	"chainguard.dev/apko/pkg/options"
)

// BoundedPool is a sync.Pool with a configurable maximum size.
// See internal/pool.BoundedPool for full documentation.
type BoundedPool = pool.BoundedPool

// PoolStats contains statistics about a BoundedPool.
type PoolStats = pool.Stats

// NewBoundedPool creates a new bounded pool with the specified maximum size.
func NewBoundedPool(maxSize int, newFunc func() any) *BoundedPool {
	return pool.NewBoundedPool(maxSize, newFunc)
}

// RegisterPool registers a named pool with the global registry.
func RegisterPool(name string, p *BoundedPool) {
	pool.Register(name, p)
}

// GetRegisteredPool retrieves a named pool from the global registry.
func GetRegisteredPool(name string) (*BoundedPool, bool) {
	return pool.GetPool(name)
}

// AllPoolStats returns statistics for all registered pools.
func AllPoolStats() map[string]PoolStats {
	return pool.AllStats()
}

// ClearPools triggers garbage collection to clear sync.Pool contents
// and resets pool counters. This is useful between builds in long-running
// services to prevent unbounded memory growth.
func ClearPools() {
	pool.ClearAll()
}

// ResetPoolMetrics resets the hit/miss/drop counters for all registered pools.
func ResetPoolMetrics() {
	pool.ResetAllMetrics()
}

// ConfigurePoolsFromOptions configures all registered pool sizes based on the provided options.
// This should be called early in service initialization, before any builds start.
// Pool sizes of 0 mean uncapped (standard sync.Pool behavior).
//
// Example usage for apko-as-a-service:
//
//	opts := &options.Options{
//	    GzipPoolSize:      options.RecommendedGzipPoolSize,      // 10
//	    BufioPoolSize:     options.RecommendedBufioPoolSize,     // 20
//	    ExpandAPKPoolSize: options.RecommendedExpandAPKPoolSize, // 20
//	    TarFSPoolSize:     options.RecommendedTarFSPoolSize,     // 20
//	}
//	build.ConfigurePoolsFromOptions(opts)
func ConfigurePoolsFromOptions(opts *options.Options) {
	if opts == nil {
		return
	}

	allStats := pool.AllStats()
	for name := range allStats {
		p, ok := pool.GetPool(name)
		if !ok {
			continue
		}

		var size int
		switch {
		case strings.HasPrefix(name, "pgzip-"):
			size = opts.GzipPoolSize
		case name == "bufio-writer":
			size = opts.BufioPoolSize
		case strings.HasPrefix(name, "expandapk-"):
			size = opts.ExpandAPKPoolSize
		case name == "tarfs-reader":
			size = opts.TarFSPoolSize
		}

		p.SetMaxSize(size)
	}
}

// ConfigurePoolsForService is a convenience function that configures all pools
// with the recommended sizes for running apko as a service with many parallel builds.
// This is equivalent to calling ConfigurePoolsFromOptions with all Recommended* values.
func ConfigurePoolsForService() {
	ConfigurePoolsFromOptions(&options.Options{
		GzipPoolSize:      options.RecommendedGzipPoolSize,
		BufioPoolSize:     options.RecommendedBufioPoolSize,
		ExpandAPKPoolSize: options.RecommendedExpandAPKPoolSize,
		TarFSPoolSize:     options.RecommendedTarFSPoolSize,
	})
}
