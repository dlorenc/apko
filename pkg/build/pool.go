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
	"chainguard.dev/apko/internal/pool"
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
