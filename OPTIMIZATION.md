# apko Optimization Guide

This document outlines optimization opportunities for apko when running as a service building many images concurrently. The focus is on reducing memory consumption and CPU load when building 10+ images in parallel with layering strategies.

## Table of Contents

- [Problem Context](#problem-context)
- [Current Architecture Bottlenecks](#current-architecture-bottlenecks)
- [Optimization Tiers](#optimization-tiers)
  - [Ground Level (Easiest)](#ground-level-easiest)
  - [Low Hanging Fruit](#low-hanging-fruit)
  - [High Level (Complex)](#high-level-complex)
- [Implementation Details](#implementation-details)
- [Impact Estimates](#impact-estimates)

---

## Problem Context

### Use Case
Running apko as a service capable of building 12+ images in parallel. Current naive implementation results in:
- **20-30GB memory consumption**
- **Massive CPU load** (thread explosion)

### Build Characteristics
Images are built with:
- Layering strategy: `origin` (each package becomes its own layer)
- High layer budget (many layers per image)
- Significant package overlap between builds

### Observed Package Patterns (from Wolfi)

**Universal Base** (present in ~75% of 3920 packages):
```yaml
packages:
  - build-base           # 74% of builds
  - busybox              # 70% of builds
  - ca-certificates-bundle  # 57% of builds
```

**Common Language Toolchains**:
| Language | Typical Environment Size |
|----------|-------------------------|
| Go | base + 5 packages |
| Python | base + 20 packages |
| Rust | base + 20 packages |
| Node.js | base + 15 packages |
| C/autoconf | base + 10 packages |

**Key Insight**: With 12 parallel builds, `build-base` (a ~50MB package) gets processed 12 times independently.

---

## Current Architecture Bottlenecks

### 1. Repeated APK Expansion/Indexing

**Location**: `pkg/apk/expandapk/expandapk.go:363-538`

Each `ExpandApk()` call:
1. Downloads the APK (streamed)
2. Decompresses to temp file
3. Reads entire tar to build index (`tarfs.New()`)
4. Holds index in memory

**Impact**:
```
12 parallel builds × ~50 avg packages × 2 passes per APK = ~1200 APK parses
```

### 2. Per-Build memFS

**Location**: `pkg/tarfs/fs.go`

Each build creates a full in-memory filesystem:
```go
type memFS struct {
    tree *node  // All files as tree nodes with data pointers
}
```

**Impact**: 200MB typical environment × 12 builds = 2.4GB in filesystems alone.

### 3. pgzip Thread Explosion

**Location**: `pkg/build/build_implementation.go:43-62`

```go
var pgzipThreads = min(runtime.GOMAXPROCS(0), 8)  // Up to 8 threads per writer
```

With "origin" layering:
- 12 images × 10 layers avg × 8 gzip threads = **960 potential concurrent gzip operations**
- Each gzip writer: 1MB blocks × 8 threads = 8MB per writer
- `sync.Pool` never shrinks in long-running processes

### 4. No Cross-Build Deduplication

**Location**: `pkg/apk/apk/implementation.go:66`

The global APK cache helps within a process, but:
- Each `expandPackage()` still creates new tarfs indexes
- Layer compression cache is by diffID, but layers are rebuilt per-build
- No image-level deduplication

### 5. Unbounded sync.Pool Growth

**Locations**:
- `pkg/build/build_implementation.go:53-80` - pgzipPool (8MB per writer), bufioPool (4MB per buffer)
- `pkg/apk/expandapk/expandapk.go:31-63` - slicePool, readerPool, writerPool (1MB each)
- `internal/tarfs/tarfs.go:31-41` - readerPool (1MB each)

In long-running services, pools grow unbounded and only clear at GC time.

---

## Optimization Tiers

### Ground Level (Easiest)

#### 1. On-Disk tarfs Index - IMPLEMENTED

**Status**: Implemented in this commit.

**Problem**: Every time an APK is used, we read the entire tar to build an index, even for cached packages.

**Current Flow** (`internal/tarfs/tarfs.go:207-263`):
```go
func New(ra io.ReaderAt, size int64) (*FS, error) {
    tr := tar.NewReader(cr)
    for {
        hdr, err := tr.Next()  // Sequential read of entire tar
        // Build in-memory index
    }
}
```

**Proposed Solution**: Serialize the tarfs index to disk alongside the cached APK data.

```go
// New serializable index structure
type SerializedIndex struct {
    Files []SerializedEntry `json:"files"`
    Dirs  map[string][]int  `json:"dirs"`  // dir path → indices into Files
}

type SerializedEntry struct {
    Name     string `json:"n"`
    Offset   int64  `json:"o"`
    Size     int64  `json:"s"`
    Mode     int64  `json:"m"`
    Typeflag byte   `json:"t"`
    Linkname string `json:"l,omitempty"`
}

// Load from cached index instead of scanning tar
func NewFromIndex(ra io.ReaderAt, indexPath string) (*FS, error) {
    data, err := os.ReadFile(indexPath)
    if err != nil {
        return nil, err
    }
    var idx SerializedIndex
    if err := json.Unmarshal(data, &idx); err != nil {
        return nil, err
    }
    // Reconstruct FS from index without reading tar
    return rebuildFromIndex(ra, &idx), nil
}

// Save index after initial scan
func (fs *FS) SaveIndex(path string) error {
    idx := fs.serialize()
    data, _ := json.Marshal(idx)
    return os.WriteFile(path, data, 0644)
}
```

**Cache File Layout**:
```
/cache/
  <pkg-checksum>.ctl.tar.gz      # Control data (existing)
  <pkg-checksum>.dat.tar.gz      # Package data (existing)
  <pkg-checksum>.dat.tar         # Uncompressed tar (existing)
  <pkg-checksum>.dat.idx         # NEW: Serialized tarfs index
```

**Key Files to Modify**:
- `internal/tarfs/tarfs.go` - Add `SerializedIndex`, `NewFromIndex()`, `SaveIndex()`
- `pkg/apk/apk/implementation.go:1173-1267` - Load cached index in `cachedPackage()`
- `pkg/apk/apk/implementation.go:1108-1171` - Save index in `cachePackage()`

**Implementation Summary**:
- Added `SerializedIndex` and `SerializedEntry` types to `internal/tarfs/tarfs.go`
- Added `SaveIndex()` method to serialize tarfs index to JSON
- Added `NewFromIndex()` function to load tarfs from cached index
- Modified `cachePackage()` to save `.idx` file alongside cached tar
- Modified `cachedPackage()` to try loading from cached index first

**Benchmark Results** (1000 file tar):
| Metric | `New()` (scan tar) | `NewFromIndex()` (load index) | Improvement |
|--------|-------------------|------------------------------|-------------|
| Time | 2.43ms | 0.87ms | **2.8x faster** |
| Memory | 1.87MB | 1.30MB | **31% less** |
| Allocations | 27,113 | 8,302 | **69% fewer** |

---

#### 2. Concurrency Tuning - IMPLEMENTED

**Status**: Implemented in this commit.

**Problem**: pgzip and APK-level concurrency assume single-image builds or giant machines.

**Implementation**:
- Added `GzipConcurrency`, `APKFetchWorkers`, `APKInstallWorkers` fields to `pkg/options/options.go`
- Added `WithGzipConcurrency()`, `WithAPKFetchWorkers()`, `WithAPKInstallWorkers()`, `WithConcurrencyLimits()` options to `pkg/build/options.go`
- Added `WithFetchWorkers()`, `WithInstallWorkers()` options to `pkg/apk/apk/options.go`
- Modified `pkg/build/build_implementation.go` to use per-concurrency-level gzip pools
- Modified `pkg/apk/apk/implementation.go` to use configurable fetch workers

**Usage**:
```go
opts := []build.Option{
    build.WithGzipConcurrency(1),      // Single-threaded gzip per layer
    build.WithAPKFetchWorkers(4),      // Limit parallel APK fetches
}
// Or use the convenience function:
opts := []build.Option{
    build.WithConcurrencyLimits(1, 4, 1),
}
```

**Original Proposed Solution**: Make concurrency configurable via `options.Options`.

```go
// pkg/options/options.go
type Options struct {
    // ... existing fields ...

    // GzipConcurrency controls pgzip parallelism per layer.
    // Default: min(GOMAXPROCS, 8). Set to 1-2 for concurrent builds.
    GzipConcurrency int

    // APKFetchWorkers controls parallel APK downloads/expansions.
    // Default: GOMAXPROCS. Set to 4-8 for concurrent builds.
    APKFetchWorkers int

    // APKInstallWorkers controls parallel package installations.
    // Default: GOMAXPROCS. Set to 1 for memory-constrained environments.
    APKInstallWorkers int
}

var Default = Options{
    GzipConcurrency:   0,  // 0 = use default (min(GOMAXPROCS, 8))
    APKFetchWorkers:   0,  // 0 = use default (GOMAXPROCS)
    APKInstallWorkers: 0,  // 0 = use default (GOMAXPROCS)
}
```

**Usage in build code**:
```go
// pkg/build/build_implementation.go
func getGzipConcurrency(o *options.Options) int {
    if o.GzipConcurrency > 0 {
        return o.GzipConcurrency
    }
    return min(runtime.GOMAXPROCS(0), 8)
}

// Create per-build gzip writer instead of global pool
func (bc *Context) newGzipWriter(w io.Writer) *gzip.Writer {
    zw := gzip.NewWriter(w)
    threads := getGzipConcurrency(&bc.o)
    zw.SetConcurrency(1<<20, threads)
    return zw
}
```

**Recommended Settings for apko-as-a-service**:
```go
opts := []build.Option{
    build.WithGzipConcurrency(1),      // Single-threaded gzip
    build.WithAPKFetchWorkers(4),      // Limit parallel downloads
    build.WithAPKInstallWorkers(1),    // Sequential installation
}
```

---

#### 3. Memory Tuning (Pool Management) - IMPLEMENTED

**Status**: Implemented in this commit.

**Problem**: `sync.Pool` instances grow unbounded in long-running processes.

**Original Pools**:
| Pool | Location | Size Per Item |
|------|----------|---------------|
| pgzipPool | build_implementation.go:53 | ~8MB (1MB × 8 threads) |
| bufioPool | build_implementation.go:70 | 4MB |
| slicePool | expandapk.go:31 | 1MB |
| readerPool | expandapk.go:41 | 1MB |
| writerPool | expandapk.go:53 | 1MB |
| readerPool | tarfs.go:31 | 1MB |

**Implementation**: Bounded pools with metrics tracking and centralized clearing.

**Key Files Modified**:
- `internal/pool/pool.go` - Core `BoundedPool` implementation and `Registry`
- `pkg/build/pool.go` - Public API re-export
- `pkg/build/build_implementation.go` - Updated pgzipPools and bufioPool
- `pkg/apk/expandapk/expandapk.go` - Updated slicePool, readerPool, writerPool
- `internal/tarfs/tarfs.go` - Updated readerPool

**Pool Size Limits** (configurable via `options.Options`):
| Pool | Default | Recommended | Memory Cap |
|------|---------|-------------|------------|
| pgzip-concurrency-N | 0 (uncapped) | 10 | ~80MB per level |
| bufio-writer | 0 (uncapped) | 20 | ~80MB |
| expandapk-slice | 0 (uncapped) | 20 | ~20MB |
| expandapk-reader | 0 (uncapped) | 20 | ~20MB |
| expandapk-writer | 0 (uncapped) | 20 | ~20MB |
| tarfs-reader | 0 (uncapped) | 20 | ~20MB |

**Usage**:
```go
import (
    "chainguard.dev/apko/pkg/build"
    "chainguard.dev/apko/pkg/options"
)

// Option 1: Configure pools for service mode (uses recommended sizes)
build.ConfigurePoolsForService()

// Option 2: Configure with custom sizes via options
opts := &options.Options{
    GzipPoolSize:      options.RecommendedGzipPoolSize,      // 10
    BufioPoolSize:     options.RecommendedBufioPoolSize,     // 20
    ExpandAPKPoolSize: options.RecommendedExpandAPKPoolSize, // 20
    TarFSPoolSize:     options.RecommendedTarFSPoolSize,     // 20
}
build.ConfigurePoolsFromOptions(opts)

// Option 3: Use 0 for any pool size to leave it uncapped (default behavior)
opts := &options.Options{
    GzipPoolSize:      5,  // Limit gzip pools
    BufioPoolSize:     0,  // Leave bufio uncapped
    ExpandAPKPoolSize: 10, // Limit expandapk pools
    TarFSPoolSize:     0,  // Leave tarfs uncapped
}
build.ConfigurePoolsFromOptions(opts)

// Clear all registered pools between builds (triggers GC)
build.ClearPools()

// Get pool statistics for monitoring
stats := build.AllPoolStats()
for name, s := range stats {
    log.Printf("%s: hits=%d misses=%d drops=%d hitRate=%.1f%%",
        name, s.Hits, s.Misses, s.Drops, s.HitRate())
}

// Reset metrics for new monitoring period
build.ResetPoolMetrics()
```

**Benchmark Results**:
| Metric | sync.Pool | BoundedPool | Overhead |
|--------|-----------|-------------|----------|
| Single-thread Get/Put | 5.6ns | 7.0ns | 25% |
| Parallel Get/Put | 1.2ns | 302ns | Higher due to atomics |
| Hit Rate (12 concurrent) | N/A | 99.9% | Excellent reuse |

**Notes**:
- The parallel overhead is acceptable since actual build operations (gzip, tar) take milliseconds
- Pool metrics are tracked via atomic counters for hit/miss/drop statistics
- `ClearPools()` forces GC and resets all pool counters

---

### Low Hanging Fruit

#### 4. Layer Compression Cache Enhancement - IMPLEMENTED

**Status**: Implemented in this commit.

**Problem**: Identical layers get compressed multiple times across builds.

**Original Cache**:
- Stored only descriptor (digest, size) keyed by diffID
- Avoided recomputing digest for same diffID
- But still recompressed every time `Compressed()` was called

**Enhanced Cache**:
- Stores both descriptor AND compressed file path
- When a layer with the same diffID needs compression, reuses the existing compressed file
- Falls back to recompression if the cached file is deleted (eviction)
- Tracks cache statistics (hits, misses, evictions)

**Key Files Modified**:
- `pkg/build/build.go` - Enhanced `compressionCacheEntry`, added stats tracking
- `pkg/build/layer_cache_test.go` - Updated tests for new behavior

**Usage**:
```go
import "chainguard.dev/apko/pkg/build"

// Get compression cache statistics
stats := build.GetCompressionCacheStats()
log.Printf("Compression cache: hits=%d misses=%d evictions=%d",
    stats.Hits, stats.Misses, stats.Evictions)

// Reset statistics for new monitoring period
build.ResetCompressionCacheStats()

// Clear the cache (for long-running services)
build.ClearCompressionCache()
```

**Impact**:
- **Before**: Each layer instance with same diffID recompressed independently
- **After**: First compression cached, subsequent layers reuse the compressed file
- **Savings**: For 12 parallel builds with ~50% package overlap, ~50% fewer compressions

---

#### 5. Image-Level Cache

**Problem**: Building the same image configuration multiple times.

**Proposed Solution**: Cache by configuration hash.

```go
// pkg/build/cache.go

type ImageCache struct {
    mu    sync.RWMutex
    cache map[string]*CachedImage  // configHash → result

    // Coalescing: multiple requests for same config wait for first build
    inflight map[string]*inflightBuild
}

type CachedImage struct {
    Digest    v1.Hash
    ConfigHash string
    CreatedAt time.Time
    Layers    []v1.Descriptor
}

type inflightBuild struct {
    done   chan struct{}
    result *CachedImage
    err    error
}

func (c *ImageCache) GetOrBuild(ctx context.Context, configHash string, buildFn func() (*CachedImage, error)) (*CachedImage, error) {
    // Check cache
    c.mu.RLock()
    if cached, ok := c.cache[configHash]; ok {
        c.mu.RUnlock()
        return cached, nil
    }

    // Check if build is in-flight
    if inflight, ok := c.inflight[configHash]; ok {
        c.mu.RUnlock()
        <-inflight.done
        return inflight.result, inflight.err
    }
    c.mu.RUnlock()

    // Start new build with coalescing
    c.mu.Lock()
    // Double-check after acquiring write lock
    if cached, ok := c.cache[configHash]; ok {
        c.mu.Unlock()
        return cached, nil
    }

    inflight := &inflightBuild{done: make(chan struct{})}
    c.inflight[configHash] = inflight
    c.mu.Unlock()

    // Do the build
    result, err := buildFn()

    // Store result
    c.mu.Lock()
    if err == nil {
        c.cache[configHash] = result
    }
    delete(c.inflight, configHash)
    inflight.result = result
    inflight.err = err
    close(inflight.done)
    c.mu.Unlock()

    return result, err
}
```

**Config Hash Computation** (already exists in `pkg/build/types/image_configuration.go:198`):
```go
func (ic *ImageConfiguration) Hash() string {
    h := sha256.New()
    // Include all deterministic config elements
    enc := json.NewEncoder(h)
    enc.Encode(ic.Contents)
    enc.Encode(ic.Accounts)
    enc.Encode(ic.Paths)
    enc.Encode(ic.Environment)
    // ... etc
    return hex.EncodeToString(h.Sum(nil))
}
```

---

#### 6. Shared tarfs Index Cache

**Problem**: Each build creates its own tarfs index for the same packages.

**Proposed Solution**: Global cache of tarfs indexes.

```go
// pkg/apk/apk/tarfs_cache.go

var tarfsIndexCache = &TarFSCache{
    cache: make(map[string]*tarfs.FS),
}

type TarFSCache struct {
    mu    sync.RWMutex
    cache map[string]*tarfs.FS  // packageChecksum → tarfs
}

func (c *TarFSCache) GetOrCreate(checksum string, create func() (*tarfs.FS, error)) (*tarfs.FS, error) {
    c.mu.RLock()
    if fs, ok := c.cache[checksum]; ok {
        c.mu.RUnlock()
        return fs, nil
    }
    c.mu.RUnlock()

    c.mu.Lock()
    defer c.mu.Unlock()

    // Double-check
    if fs, ok := c.cache[checksum]; ok {
        return fs, nil
    }

    fs, err := create()
    if err != nil {
        return nil, err
    }

    c.cache[checksum] = fs
    return fs, nil
}

// Clear old entries periodically
func (c *TarFSCache) Evict(olderThan time.Duration) {
    // Implementation with LRU or time-based eviction
}
```

**Integration** (`pkg/apk/expandapk/expandapk.go`):
```go
func ExpandApk(ctx context.Context, source io.Reader, cacheDir string) (*APKExpanded, error) {
    // ... existing code ...

    // Use cached tarfs if available
    checksum := hex.EncodeToString(exp.PackageHash)
    exp.TarFS, err = tarfsIndexCache.GetOrCreate(checksum, func() (*tarfs.FS, error) {
        data, err := exp.PackageData()
        if err != nil {
            return nil, err
        }
        info, err := data.Stat()
        if err != nil {
            return nil, err
        }
        return tarfs.New(data, info.Size())
    })

    return &exp, nil
}
```

---

### High Level (Complex)

#### 7. Shared memFS with Copy-on-Write

**Problem**: Each build creates a complete memFS copy of all packages.

**Proposed Solution**: Shared immutable base with per-build overlays.

```go
// pkg/tarfs/shared_fs.go

type SharedFS struct {
    base     *memFS              // Immutable shared state
    overlay  *memFS              // Per-build modifications
    overlaid map[string]struct{} // Paths that have been modified
}

func NewSharedFS(base *memFS) *SharedFS {
    return &SharedFS{
        base:     base,
        overlay:  New(),
        overlaid: make(map[string]struct{}),
    }
}

func (s *SharedFS) Open(name string) (fs.File, error) {
    // Check overlay first
    if _, modified := s.overlaid[name]; modified {
        return s.overlay.Open(name)
    }
    return s.base.Open(name)
}

func (s *SharedFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
    s.overlaid[name] = struct{}{}
    return s.overlay.WriteFile(name, data, perm)
}

// ... other methods follow same pattern
```

**Base FS Creation**:
```go
// Build a shared base from common packages
func BuildSharedBase(packages []string) (*memFS, error) {
    base := New()
    for _, pkg := range packages {
        // Install package into base
    }
    // Freeze base (make immutable)
    return base, nil
}
```

**Usage**:
```go
// At service startup
sharedBase, _ := BuildSharedBase([]string{"build-base", "busybox", "ca-certificates-bundle"})

// For each build
buildFS := NewSharedFS(sharedBase)
// Install additional packages into buildFS
```

---

#### 8. Compositional FS Caching (PhD-level)

**Concept**: Cache filesystem states as composable transforms.

```
State = hash(accounts) | hash(paths) | hash(pkg[0]) | ... | hash(pkg[n])
```

**Key Insight**: Installing `build-base` on an empty FS always produces the same result. Cache it.

```go
// pkg/build/compositional_cache.go

type FSState struct {
    Hash   string
    Parent *FSState  // nil for root
    Op     Operation // What was applied to parent
}

type Operation interface {
    Apply(fs *memFS) error
    Hash() string
}

type InstallPackageOp struct {
    Package *apk.Package
    Files   []tar.Header
}

func (op *InstallPackageOp) Hash() string {
    h := sha256.New()
    h.Write([]byte(op.Package.Checksum))
    return hex.EncodeToString(h.Sum(nil))
}

// Cache: stateHash → serialized memFS
type CompositionalCache struct {
    states map[string]*FSState
    data   map[string][]byte  // Serialized memFS snapshots
}

func (c *CompositionalCache) GetOrApply(parent *FSState, op Operation, fs *memFS) (*FSState, error) {
    newHash := hashState(parent, op)

    if state, ok := c.states[newHash]; ok {
        // Load cached state
        return state, c.loadInto(fs, newHash)
    }

    // Apply operation
    if err := op.Apply(fs); err != nil {
        return nil, err
    }

    // Cache result
    state := &FSState{Hash: newHash, Parent: parent, Op: op}
    c.states[newHash] = state
    c.data[newHash] = c.serialize(fs)

    return state, nil
}
```

**Benefits**:
- Two builds needing `[build-base, busybox, go]` compute it once
- Incremental: adding one package to a cached base is fast
- Reproducible: same inputs always produce same state

**Challenges**:
- Defining canonical operation ordering
- Handling non-deterministic package installations
- Storage costs for many states
- Cache invalidation when packages update

---

## Implementation Details

### File Locations Reference

| Component | File |
|-----------|------|
| APK expansion | `pkg/apk/expandapk/expandapk.go` |
| tarfs implementation | `internal/tarfs/tarfs.go` |
| memFS implementation | `pkg/tarfs/fs.go` |
| Layer creation | `pkg/build/build.go` |
| Compression | `pkg/build/build_implementation.go` |
| APK installation | `pkg/apk/apk/implementation.go` |
| Build options | `pkg/options/options.go` |
| Image configuration | `pkg/build/types/image_configuration.go` |

### Recommended Implementation Order

1. **Week 1**: Concurrency controls + pool limits
   - Immediate relief for CPU/memory pressure
   - Low risk, easily configurable

2. **Week 2**: Shared tarfs index cache
   - Major memory reduction for common packages
   - Moderate complexity

3. **Week 3-4**: On-disk tarfs index
   - Eliminates repeated tar scans
   - Medium complexity, needs serialization format

4. **Month 2**: Layer cache enhancement
   - Skip compression for known layers
   - Requires integration with ggcr

5. **Month 3+**: Image cache + compositional FS
   - Full deduplication
   - Higher complexity, significant refactoring

---

## Impact Estimates

### Per-Optimization Impact

| Optimization | Memory Reduction | CPU Reduction | Implementation Effort | Status |
|--------------|------------------|---------------|----------------------|--------|
| Concurrency controls | 30-50% | 60-80% | 1-2 days | **DONE** |
| Pool size limits | 10-20% | 0% | 1 day | **DONE** |
| Shared tarfs cache | 40-60% | 20-30% | 2-3 days | Pending |
| On-disk tarfs index | 20-30% | 30-40% | 1 week | **DONE** |
| Layer skip-compress | 10-20% | 40-60% | 1 week | Pending |
| Image-level cache | 50-80%* | 50-80%* | 1-2 weeks | Pending |
| Shared memFS | 60-80% | 10% | 2-4 weeks | Pending |
| Compositional cache | 70-90%* | 70-90%* | 1-2 months | Pending |

*For repeated builds of same/similar configs

### Combined Impact for 12 Parallel Builds

**Current State**: 20-30GB memory, CPU saturation

**After Implemented Optimizations (Concurrency + Pools + On-disk tarfs index)**:
- Memory: 10-15GB (50% reduction)
- CPU: Manageable load (70% reduction in thread contention)
- Pool memory capped at ~240MB total across all pools

**After Tier 1 (+ Shared tarfs cache)**:
- Memory: 8-12GB (60% reduction)
- CPU: Improved further with shared index cache

**After Tier 2 (+ On-disk index + Layer cache)**:
- Memory: 5-8GB (75% reduction)
- CPU: Efficient utilization (minimal redundant work)

**After Tier 3 (+ Image cache + Shared memFS)**:
- Memory: 2-4GB (90% reduction)
- CPU: Near-optimal (full deduplication)

---

## Configuration Recommendations

### For apko-as-a-service (12+ parallel builds)

```go
opts := []build.Option{
    // Limit gzip threads to prevent CPU explosion
    build.WithGzipConcurrency(1),

    // Limit parallel APK fetches
    build.WithAPKFetchWorkers(4),

    // Sequential package installation (memory-friendly)
    build.WithAPKInstallWorkers(1),

    // Enable shared caches
    build.WithSharedTarFSCache(true),
    build.WithLayerCompressionCache(true),

    // Enable image-level caching
    build.WithImageCache(imageCache),
}
```

### For CLI (single build, fast as possible)

```go
opts := []build.Option{
    // Use defaults - maximize parallelism
    // GzipConcurrency: min(GOMAXPROCS, 8)
    // APKFetchWorkers: GOMAXPROCS
}
```

---

## Appendix: Profiling Commands

### Memory Profiling
```bash
go tool pprof http://localhost:6060/debug/pprof/heap
```

### CPU Profiling
```bash
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30
```

### Goroutine Analysis
```bash
go tool pprof http://localhost:6060/debug/pprof/goroutine
```

### Key Metrics to Watch
- `go_memstats_heap_inuse_bytes` - Active heap memory
- `go_memstats_heap_objects` - Number of allocated objects
- `go_goroutines` - Number of goroutines (watch for explosion)
- Custom: layers compressed, cache hit rate, APK parse time
