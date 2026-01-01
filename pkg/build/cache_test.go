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
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/stretchr/testify/require"
)

func TestImageCache_GetOrBuild_Miss(t *testing.T) {
	cache := NewImageCache()
	ctx := context.Background()

	buildCalled := false
	result, err := cache.GetOrBuild(ctx, "test-hash", func(ctx context.Context) (v1.Image, error) {
		buildCalled = true
		return empty.Image, nil
	})

	require.NoError(t, err)
	require.True(t, buildCalled, "build function should be called on cache miss")
	require.NotNil(t, result)
	require.Equal(t, "test-hash", result.ConfigHash)

	stats := cache.Stats()
	require.Equal(t, int64(0), stats.Hits)
	require.Equal(t, int64(1), stats.Misses)
	require.Equal(t, 1, stats.Size)
}

func TestImageCache_GetOrBuild_Hit(t *testing.T) {
	cache := NewImageCache()
	ctx := context.Background()

	// First call - cache miss
	_, err := cache.GetOrBuild(ctx, "test-hash", func(ctx context.Context) (v1.Image, error) {
		return empty.Image, nil
	})
	require.NoError(t, err)

	// Second call - should be cache hit
	buildCalled := false
	result, err := cache.GetOrBuild(ctx, "test-hash", func(ctx context.Context) (v1.Image, error) {
		buildCalled = true
		return empty.Image, nil
	})

	require.NoError(t, err)
	require.False(t, buildCalled, "build function should NOT be called on cache hit")
	require.NotNil(t, result)

	stats := cache.Stats()
	require.Equal(t, int64(1), stats.Hits)
	require.Equal(t, int64(1), stats.Misses)
}

func TestImageCache_GetOrBuild_Error(t *testing.T) {
	cache := NewImageCache()
	ctx := context.Background()

	expectedErr := errors.New("build failed")
	result, err := cache.GetOrBuild(ctx, "test-hash", func(ctx context.Context) (v1.Image, error) {
		return nil, expectedErr
	})

	require.Error(t, err)
	require.Equal(t, expectedErr, err)
	require.Nil(t, result)

	// Verify nothing was cached
	stats := cache.Stats()
	require.Equal(t, 0, stats.Size)
}

func TestImageCache_GetOrBuild_Coalescing(t *testing.T) {
	cache := NewImageCache()
	ctx := context.Background()

	var buildCount atomic.Int32
	buildStarted := make(chan struct{})
	buildComplete := make(chan struct{})

	// Start first build that will block
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, err := cache.GetOrBuild(ctx, "test-hash", func(ctx context.Context) (v1.Image, error) {
			buildCount.Add(1)
			close(buildStarted)
			<-buildComplete
			return empty.Image, nil
		})
		require.NoError(t, err)
	}()

	// Wait for first build to start
	<-buildStarted

	// Start second request for same hash - should coalesce
	go func() {
		defer wg.Done()
		_, err := cache.GetOrBuild(ctx, "test-hash", func(ctx context.Context) (v1.Image, error) {
			buildCount.Add(1)
			return empty.Image, nil
		})
		require.NoError(t, err)
	}()

	// Give second goroutine time to start waiting
	time.Sleep(50 * time.Millisecond)

	// Complete the build
	close(buildComplete)
	wg.Wait()

	// Verify build was only called once
	require.Equal(t, int32(1), buildCount.Load(), "build should only be called once due to coalescing")

	stats := cache.Stats()
	require.Equal(t, int64(1), stats.Coalesced, "one request should have been coalesced")
}

func TestImageCache_GetOrBuild_ContextCancellation(t *testing.T) {
	cache := NewImageCache()

	buildStarted := make(chan struct{})
	buildComplete := make(chan struct{})

	// Start a long-running build
	go func() {
		ctx := context.Background()
		cache.GetOrBuild(ctx, "test-hash", func(ctx context.Context) (v1.Image, error) {
			close(buildStarted)
			<-buildComplete
			return empty.Image, nil
		})
	}()

	<-buildStarted

	// Create a context that we'll cancel
	ctx, cancel := context.WithCancel(context.Background())

	// Start second request with cancellable context
	errChan := make(chan error, 1)
	go func() {
		_, err := cache.GetOrBuild(ctx, "test-hash", func(ctx context.Context) (v1.Image, error) {
			return empty.Image, nil
		})
		errChan <- err
	}()

	// Give it time to start waiting
	time.Sleep(50 * time.Millisecond)

	// Cancel the context
	cancel()

	// Should get context cancelled error
	err := <-errChan
	require.ErrorIs(t, err, context.Canceled)

	// Complete the original build
	close(buildComplete)
}

func TestImageCache_Put(t *testing.T) {
	cache := NewImageCache()

	result := cache.Put("test-hash", empty.Image)

	require.NotNil(t, result)
	require.Equal(t, "test-hash", result.ConfigHash)

	// Verify it's in the cache
	cached, ok := cache.Get("test-hash")
	require.True(t, ok)
	require.Equal(t, result, cached)
}

func TestImageCache_Delete(t *testing.T) {
	cache := NewImageCache()
	cache.Put("test-hash", empty.Image)

	cache.Delete("test-hash")

	_, ok := cache.Get("test-hash")
	require.False(t, ok)
}

func TestImageCache_Clear(t *testing.T) {
	cache := NewImageCache()
	cache.Put("hash1", empty.Image)
	cache.Put("hash2", empty.Image)
	cache.Put("hash3", empty.Image)

	require.Equal(t, 3, cache.Size())

	cache.Clear()

	require.Equal(t, 0, cache.Size())
}

func TestImageCache_Evict(t *testing.T) {
	cache := NewImageCache()

	// Add some entries with different timestamps
	cache.Put("hash1", empty.Image)
	time.Sleep(100 * time.Millisecond)
	cache.Put("hash2", empty.Image)
	time.Sleep(100 * time.Millisecond)
	cache.Put("hash3", empty.Image)

	require.Equal(t, 3, cache.Size())

	// Evict entries older than 150ms
	// hash1 (200ms old) and hash2 (100ms old) should be evicted
	// hash3 (just added) should remain
	evicted := cache.Evict(150 * time.Millisecond)

	// Only hash1 should be evicted (>150ms old)
	require.GreaterOrEqual(t, evicted, 1, "at least one entry should be evicted")
	require.Less(t, cache.Size(), 3, "cache should have fewer than 3 entries")
}

func TestImageCache_ResetStats(t *testing.T) {
	cache := NewImageCache()
	ctx := context.Background()

	// Generate some stats
	cache.GetOrBuild(ctx, "hash1", func(ctx context.Context) (v1.Image, error) {
		return empty.Image, nil
	})
	cache.GetOrBuild(ctx, "hash1", func(ctx context.Context) (v1.Image, error) {
		return empty.Image, nil
	})

	stats := cache.Stats()
	require.Equal(t, int64(1), stats.Hits)
	require.Equal(t, int64(1), stats.Misses)

	cache.ResetStats()

	stats = cache.Stats()
	require.Equal(t, int64(0), stats.Hits)
	require.Equal(t, int64(0), stats.Misses)
	require.Equal(t, int64(0), stats.Coalesced)
}

func TestImageCache_DifferentHashes(t *testing.T) {
	cache := NewImageCache()
	ctx := context.Background()

	// Build with different hashes should each trigger their own build
	buildCount := 0
	for i := 0; i < 5; i++ {
		hash := "hash-" + string(rune('a'+i))
		_, err := cache.GetOrBuild(ctx, hash, func(ctx context.Context) (v1.Image, error) {
			buildCount++
			return empty.Image, nil
		})
		require.NoError(t, err)
	}

	require.Equal(t, 5, buildCount)
	require.Equal(t, 5, cache.Size())

	stats := cache.Stats()
	require.Equal(t, int64(0), stats.Hits)
	require.Equal(t, int64(5), stats.Misses)
}

func TestDefaultImageCache(t *testing.T) {
	// Test that DefaultImageCache returns a singleton
	cache1 := DefaultImageCache()
	cache2 := DefaultImageCache()

	require.Same(t, cache1, cache2)
}
