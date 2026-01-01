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
	"context"
	sha2562 "crypto/sha256"
	"encoding/base64"
	"fmt"
	"maps"
	"net/http"
	"time"

	"chainguard.dev/apko/pkg/apk/apk"
	"chainguard.dev/apko/pkg/apk/auth"
	"chainguard.dev/apko/pkg/build/types"

	"github.com/chainguard-dev/clog"
)

// Option is an option for the build context.
type Option func(*Context) error

// WithConfig sets the image configuration for the build context.
// The image configuration is parsed from given config file.
// TODO(jason): Remove this.
func WithConfig(configFile string, includePaths []string) Option {
	return func(bc *Context) error {
		ctx := context.Background()
		log := clog.FromContext(ctx)
		log.Debugf("loading config file: %s", configFile)

		var ic types.ImageConfiguration
		hasher := sha2562.New()
		if err := ic.Load(ctx, configFile, includePaths, hasher); err != nil { //nolint:staticcheck
			return fmt.Errorf("failed to load image configuration: %w", err)
		}

		bc.ic = ic
		bc.o.ImageConfigFile = configFile
		bc.o.ImageConfigChecksum = "sha256-" + base64.StdEncoding.EncodeToString(hasher.Sum(nil))

		return nil
	}
}

// WithTags sets the tags for the build context.
func WithTags(tags ...string) Option {
	return func(bc *Context) error {
		bc.o.Tags = tags
		return nil
	}
}

// WithTarball sets the output path of the layer tarball.
func WithTarball(path string) Option {
	return func(bc *Context) error {
		bc.o.TarballPath = path
		return nil
	}
}

// WithBuildDate sets the timestamps for the build context.
// The string is parsed according to RFC3339.
// An empty string is a special case and will default to
// the unix epoch.
func WithBuildDate(s string) Option {
	return func(bc *Context) error {
		// default to 0 for reproducibility
		if s == "" {
			bc.o.SourceDateEpoch = time.Unix(0, 0).UTC()
			return nil
		}

		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return err
		}

		bc.o.SourceDateEpoch = t

		return nil
	}
}

// WithSourceDateEpoch is like WithBuildDate but not a string.
func WithSourceDateEpoch(t time.Time) Option {
	return func(bc *Context) error {
		bc.o.SourceDateEpoch = t
		return nil
	}
}

func WithSBOM(path string) Option {
	return func(bc *Context) error {
		bc.o.SBOMPath = path
		return nil
	}
}

func WithSBOMFormats(formats []string) Option {
	return func(bc *Context) error {
		bc.o.SBOMFormats = formats
		return nil
	}
}

func WithExtraKeys(keys []string) Option {
	return func(bc *Context) error {
		bc.o.ExtraKeyFiles = keys
		return nil
	}
}

func WithExtraBuildRepos(repos []string) Option {
	return func(bc *Context) error {
		bc.o.ExtraBuildRepos = repos
		return nil
	}
}

func WithExtraRepos(repos []string) Option {
	return func(bc *Context) error {
		bc.o.ExtraRepos = repos
		return nil
	}
}

func WithExtraPackages(packages []string) Option {
	return func(bc *Context) error {
		bc.o.ExtraPackages = packages
		return nil
	}
}

func WithIncludePaths(includePaths []string) Option {
	return func(bc *Context) error {
		bc.o.IncludePaths = includePaths
		return nil
	}
}

// WithImageConfiguration sets the ImageConfiguration object
// to use when building.
func WithImageConfiguration(ic types.ImageConfiguration) Option {
	return func(bc *Context) error {
		bc.ic = ic
		return nil
	}
}

// WithArch sets the architecture for the build context.
func WithArch(arch types.Architecture) Option {
	return func(bc *Context) error {
		bc.o.Arch = arch
		return nil
	}
}

// WithVCS enables VCS URL probing for the build context.
func WithVCS(enable bool) Option {
	return func(bc *Context) error {
		bc.o.WithVCS = enable
		return nil
	}
}

// WithAnnotations adds annotations from commandline to those in the config.
// Commandline annotations take precedence.
func WithAnnotations(annotations map[string]string) Option {
	return func(bc *Context) error {
		if bc.ic.Annotations == nil {
			bc.ic.Annotations = make(map[string]string)
		}
		maps.Copy(bc.ic.Annotations, annotations)
		return nil
	}
}

// WithCache set the cache directory to use
func WithCache(cacheDir string, offline bool, shared *apk.Cache) Option {
	return func(bc *Context) error {
		bc.o.CacheDir = cacheDir
		bc.o.Offline = offline
		bc.o.SharedCache = shared
		return nil
	}
}

func WithLockFile(lockFile string) Option {
	return func(bc *Context) error {
		bc.o.Lockfile = lockFile
		return nil
	}
}

func WithTempDir(tmp string) Option {
	return func(bc *Context) error {
		bc.o.TempDirPath = tmp
		return nil
	}
}

func WithAuthenticator(a auth.Authenticator) Option {
	return func(bc *Context) error {
		bc.o.Auth = a
		return nil
	}
}

// WithIgnoreSignatures sets whether to ignore repository signature verification.
// Default is false.
func WithIgnoreSignatures(ignore bool) Option {
	return func(bc *Context) error {
		bc.o.IgnoreSignatures = ignore
		return nil
	}
}

// WithTransport allows explicitly setting the inner HTTP transport.
func WithTransport(t http.RoundTripper) Option {
	return func(bc *Context) error {
		bc.o.Transport = t
		return nil
	}
}

// WithGzipConcurrency sets the number of parallel threads used by pgzip for layer compression.
// When set to 0 (default), uses min(GOMAXPROCS, 8) threads per compression operation.
//
// For services running many parallel builds (e.g., apko-as-a-service), set to 1-2 to prevent
// thread explosion. With 12 parallel builds using default settings, you could have up to
// 12 * 8 = 96 concurrent gzip threads competing for CPU.
//
// Each gzip writer uses approximately (concurrency * 1MB) of memory for buffering,
// so reducing this value also reduces memory consumption.
//
// Example for a service building 12 images in parallel:
//
//	opts := []build.Option{
//	    build.WithGzipConcurrency(1),  // Single-threaded compression per layer
//	}
func WithGzipConcurrency(concurrency int) Option {
	return func(bc *Context) error {
		bc.o.GzipConcurrency = concurrency
		return nil
	}
}

// WithAPKFetchWorkers sets the number of parallel goroutines for fetching and expanding APK packages.
// When set to 0 (default), uses GOMAXPROCS workers.
//
// Each APK fetch operation involves network I/O, decompression, and tarfs indexing.
// For services running many parallel builds, set to 4-8 to limit concurrent operations
// and prevent memory pressure from many simultaneous APK expansions.
//
// Example for a service building 12 images in parallel:
//
//	opts := []build.Option{
//	    build.WithAPKFetchWorkers(4),  // Limit parallel APK fetches
//	}
func WithAPKFetchWorkers(workers int) Option {
	return func(bc *Context) error {
		bc.o.APKFetchWorkers = workers
		return nil
	}
}

// WithAPKInstallWorkers sets the number of parallel goroutines for installing APK packages.
// When set to 0 (default), uses GOMAXPROCS workers.
//
// Package installation involves writing files to the in-memory filesystem.
// For memory-constrained environments or when running many parallel builds,
// set to 1 for sequential installation to reduce peak memory usage.
//
// Example for a memory-constrained service:
//
//	opts := []build.Option{
//	    build.WithAPKInstallWorkers(1),  // Sequential package installation
//	}
func WithAPKInstallWorkers(workers int) Option {
	return func(bc *Context) error {
		bc.o.APKInstallWorkers = workers
		return nil
	}
}

// WithConcurrencyLimits is a convenience function that sets all concurrency controls at once.
// This is useful for services that need to limit resource usage across all operations.
//
// Parameters:
//   - gzipConcurrency: threads per gzip operation (0 = default min(GOMAXPROCS, 8))
//   - apkFetchWorkers: parallel APK fetch goroutines (0 = default GOMAXPROCS)
//   - apkInstallWorkers: parallel APK install goroutines (0 = default GOMAXPROCS)
//
// Example for apko-as-a-service running 12+ parallel builds:
//
//	opts := []build.Option{
//	    build.WithConcurrencyLimits(1, 4, 1),  // Conservative limits
//	}
//
// This would result in:
//   - Single-threaded gzip compression (prevents 96+ gzip threads)
//   - 4 parallel APK fetches (limits network/disk I/O)
//   - Sequential package installation (reduces memory pressure)
func WithConcurrencyLimits(gzipConcurrency, apkFetchWorkers, apkInstallWorkers int) Option {
	return func(bc *Context) error {
		bc.o.GzipConcurrency = gzipConcurrency
		bc.o.APKFetchWorkers = apkFetchWorkers
		bc.o.APKInstallWorkers = apkInstallWorkers
		return nil
	}
}
