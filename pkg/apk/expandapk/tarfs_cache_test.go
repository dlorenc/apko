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
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"chainguard.dev/apko/internal/tarfs"
	"github.com/stretchr/testify/require"
)

// createTestTarFile creates a temporary tar file with test content.
func createTestTarFile(t *testing.T) string {
	t.Helper()

	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "test.tar")

	f, err := os.Create(tarPath)
	require.NoError(t, err)

	tw := tar.NewWriter(f)

	// Add a few test files
	files := []struct {
		name    string
		content string
	}{
		{"file1.txt", "content of file 1"},
		{"file2.txt", "content of file 2"},
		{"dir/file3.txt", "content of file 3"},
	}

	for _, file := range files {
		hdr := &tar.Header{
			Name: file.name,
			Mode: 0644,
			Size: int64(len(file.content)),
		}
		require.NoError(t, tw.WriteHeader(hdr))
		_, err := tw.Write([]byte(file.content))
		require.NoError(t, err)
	}

	require.NoError(t, tw.Close())
	require.NoError(t, f.Close())

	return tarPath
}

func TestTarFSCache_GetOrCreate_Miss(t *testing.T) {
	cache := NewTarFSCache()
	defer cache.Close()

	tarPath := createTestTarFile(t)
	packageHash := []byte("test-hash-1")

	createCalled := false
	fs, err := cache.GetOrCreate(packageHash, func() (*tarfs.FS, *os.File, string, error) {
		createCalled = true
		f, err := os.Open(tarPath)
		if err != nil {
			return nil, nil, "", err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, nil, "", err
		}
		tfs, err := tarfs.New(f, info.Size())
		if err != nil {
			f.Close()
			return nil, nil, "", err
		}
		return tfs, f, tarPath, nil
	})

	require.NoError(t, err)
	require.True(t, createCalled, "create function should be called on cache miss")
	require.NotNil(t, fs)

	stats := cache.Stats()
	require.Equal(t, int64(0), stats.Hits)
	require.Equal(t, int64(1), stats.Misses)
	require.Equal(t, 1, stats.Size)
}

func TestTarFSCache_GetOrCreate_Hit(t *testing.T) {
	cache := NewTarFSCache()
	defer cache.Close()

	tarPath := createTestTarFile(t)
	packageHash := []byte("test-hash-2")

	// First call - cache miss
	createCount := 0
	createFn := func() (*tarfs.FS, *os.File, string, error) {
		createCount++
		f, err := os.Open(tarPath)
		if err != nil {
			return nil, nil, "", err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, nil, "", err
		}
		tfs, err := tarfs.New(f, info.Size())
		if err != nil {
			f.Close()
			return nil, nil, "", err
		}
		return tfs, f, tarPath, nil
	}

	fs1, err := cache.GetOrCreate(packageHash, createFn)
	require.NoError(t, err)
	require.Equal(t, 1, createCount)

	// Second call - should be cache hit
	fs2, err := cache.GetOrCreate(packageHash, createFn)
	require.NoError(t, err)
	require.Equal(t, 1, createCount, "create function should NOT be called on cache hit")
	require.Same(t, fs1, fs2, "should return the same cached instance")

	stats := cache.Stats()
	require.Equal(t, int64(1), stats.Hits)
	require.Equal(t, int64(1), stats.Misses)
}

func TestTarFSCache_DifferentHashes(t *testing.T) {
	cache := NewTarFSCache()
	defer cache.Close()

	tarPath := createTestTarFile(t)

	createFn := func() (*tarfs.FS, *os.File, string, error) {
		f, err := os.Open(tarPath)
		if err != nil {
			return nil, nil, "", err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, nil, "", err
		}
		tfs, err := tarfs.New(f, info.Size())
		if err != nil {
			f.Close()
			return nil, nil, "", err
		}
		return tfs, f, tarPath, nil
	}

	// Different hashes should create different cache entries
	fs1, err := cache.GetOrCreate([]byte("hash-1"), createFn)
	require.NoError(t, err)

	fs2, err := cache.GetOrCreate([]byte("hash-2"), createFn)
	require.NoError(t, err)

	require.NotSame(t, fs1, fs2, "different hashes should have different entries")
	require.Equal(t, 2, cache.Size())

	stats := cache.Stats()
	require.Equal(t, int64(0), stats.Hits)
	require.Equal(t, int64(2), stats.Misses)
}

func TestTarFSCache_ConcurrentAccess(t *testing.T) {
	cache := NewTarFSCache()
	defer cache.Close()

	tarPath := createTestTarFile(t)
	packageHash := []byte("concurrent-hash")

	var createCount int32
	var mu sync.Mutex

	createFn := func() (*tarfs.FS, *os.File, string, error) {
		mu.Lock()
		createCount++
		mu.Unlock()

		f, err := os.Open(tarPath)
		if err != nil {
			return nil, nil, "", err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, nil, "", err
		}
		tfs, err := tarfs.New(f, info.Size())
		if err != nil {
			f.Close()
			return nil, nil, "", err
		}
		return tfs, f, tarPath, nil
	}

	// Run multiple goroutines trying to get the same hash
	var wg sync.WaitGroup
	results := make([]*tarfs.FS, 10)
	errs := make([]error, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = cache.GetOrCreate(packageHash, createFn)
		}(i)
	}

	wg.Wait()

	// All should succeed
	for i, err := range errs {
		require.NoError(t, err, "goroutine %d should succeed", i)
	}

	// All should return the same tarfs instance
	for i := 1; i < len(results); i++ {
		require.Same(t, results[0], results[i], "all goroutines should get the same cached instance")
	}

	// Create should only be called once
	require.Equal(t, int32(1), createCount, "create should only be called once")
}

func TestTarFSCache_Clear(t *testing.T) {
	cache := NewTarFSCache()

	tarPath := createTestTarFile(t)

	createFn := func() (*tarfs.FS, *os.File, string, error) {
		f, err := os.Open(tarPath)
		if err != nil {
			return nil, nil, "", err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, nil, "", err
		}
		tfs, err := tarfs.New(f, info.Size())
		if err != nil {
			f.Close()
			return nil, nil, "", err
		}
		return tfs, f, tarPath, nil
	}

	// Add entries
	cache.GetOrCreate([]byte("hash-1"), createFn)
	cache.GetOrCreate([]byte("hash-2"), createFn)
	cache.GetOrCreate([]byte("hash-3"), createFn)

	require.Equal(t, 3, cache.Size())

	cache.Clear()

	require.Equal(t, 0, cache.Size())
}

func TestTarFSCache_Delete(t *testing.T) {
	cache := NewTarFSCache()
	defer cache.Close()

	tarPath := createTestTarFile(t)

	createFn := func() (*tarfs.FS, *os.File, string, error) {
		f, err := os.Open(tarPath)
		if err != nil {
			return nil, nil, "", err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, nil, "", err
		}
		tfs, err := tarfs.New(f, info.Size())
		if err != nil {
			f.Close()
			return nil, nil, "", err
		}
		return tfs, f, tarPath, nil
	}

	hash := []byte("delete-test-hash")
	cache.GetOrCreate(hash, createFn)
	require.Equal(t, 1, cache.Size())

	cache.Delete(hash)
	require.Equal(t, 0, cache.Size())

	// Should not be found after delete
	_, found := cache.Get(hash)
	require.False(t, found)
}

func TestTarFSCache_Evict(t *testing.T) {
	cache := NewTarFSCache()
	defer cache.Close()

	tarPath := createTestTarFile(t)

	createFn := func() (*tarfs.FS, *os.File, string, error) {
		f, err := os.Open(tarPath)
		if err != nil {
			return nil, nil, "", err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, nil, "", err
		}
		tfs, err := tarfs.New(f, info.Size())
		if err != nil {
			f.Close()
			return nil, nil, "", err
		}
		return tfs, f, tarPath, nil
	}

	// Add first entry
	cache.GetOrCreate([]byte("hash-1"), createFn)
	time.Sleep(100 * time.Millisecond)

	// Add second entry
	cache.GetOrCreate([]byte("hash-2"), createFn)
	time.Sleep(100 * time.Millisecond)

	// Add third entry
	cache.GetOrCreate([]byte("hash-3"), createFn)

	require.Equal(t, 3, cache.Size())

	// Evict entries unused for more than 150ms
	evicted := cache.Evict(150 * time.Millisecond)

	require.GreaterOrEqual(t, evicted, 1, "at least one entry should be evicted")
	require.Less(t, cache.Size(), 3, "cache should have fewer than 3 entries")
}

func TestTarFSCache_ResetStats(t *testing.T) {
	cache := NewTarFSCache()
	defer cache.Close()

	tarPath := createTestTarFile(t)

	createFn := func() (*tarfs.FS, *os.File, string, error) {
		f, err := os.Open(tarPath)
		if err != nil {
			return nil, nil, "", err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, nil, "", err
		}
		tfs, err := tarfs.New(f, info.Size())
		if err != nil {
			f.Close()
			return nil, nil, "", err
		}
		return tfs, f, tarPath, nil
	}

	hash := []byte("stats-test")

	// Generate stats
	cache.GetOrCreate(hash, createFn) // miss
	cache.GetOrCreate(hash, createFn) // hit

	stats := cache.Stats()
	require.Equal(t, int64(1), stats.Hits)
	require.Equal(t, int64(1), stats.Misses)

	cache.ResetStats()

	stats = cache.Stats()
	require.Equal(t, int64(0), stats.Hits)
	require.Equal(t, int64(0), stats.Misses)
}

func TestTarFSCache_CachedTarFSIsUsable(t *testing.T) {
	cache := NewTarFSCache()
	defer cache.Close()

	tarPath := createTestTarFile(t)
	packageHash := []byte("usability-test")

	createFn := func() (*tarfs.FS, *os.File, string, error) {
		f, err := os.Open(tarPath)
		if err != nil {
			return nil, nil, "", err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, nil, "", err
		}
		tfs, err := tarfs.New(f, info.Size())
		if err != nil {
			f.Close()
			return nil, nil, "", err
		}
		return tfs, f, tarPath, nil
	}

	// Get cached tarfs
	tfs, err := cache.GetOrCreate(packageHash, createFn)
	require.NoError(t, err)

	// Verify we can read files from the cached tarfs
	f, err := tfs.Open("file1.txt")
	require.NoError(t, err)
	defer f.Close()

	content := make([]byte, 100)
	n, err := f.Read(content)
	require.NoError(t, err)
	require.Equal(t, "content of file 1", string(content[:n]))

	// Get the cached tarfs again and verify it's still usable
	tfs2, err := cache.GetOrCreate(packageHash, createFn)
	require.NoError(t, err)
	require.Same(t, tfs, tfs2)

	f2, err := tfs2.Open("file2.txt")
	require.NoError(t, err)
	defer f2.Close()

	content2 := make([]byte, 100)
	n2, err := f2.Read(content2)
	require.NoError(t, err)
	require.Equal(t, "content of file 2", string(content2[:n2]))
}

func TestGlobalTarFSCache(t *testing.T) {
	// Test that GlobalTarFSCache returns a singleton
	cache1 := GlobalTarFSCache()
	cache2 := GlobalTarFSCache()

	require.Same(t, cache1, cache2)
}

// Helper to create tar content in memory for tests that don't need files.
func createTestTarContent() []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	hdr := &tar.Header{
		Name: "test.txt",
		Mode: 0644,
		Size: 4,
	}
	tw.WriteHeader(hdr)
	tw.Write([]byte("test"))
	tw.Close()

	return buf.Bytes()
}
