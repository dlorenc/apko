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

package options

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"

	"chainguard.dev/apko/pkg/apk/apk"
	"chainguard.dev/apko/pkg/apk/auth"
	"chainguard.dev/apko/pkg/build/types"
)

type Options struct {
	WithVCS bool `json:"withVCS,omitempty"`
	// ImageConfigFile might, but does not have to be a filename. It might be any abstract configuration identifier.
	ImageConfigFile string `json:"imageConfigFile,omitempty"`
	// ImageConfigChecksum (when set) allows to detect mismatch between configuration and the lockfile.
	ImageConfigChecksum     string             `json:"configChecksum,omitempty"`
	TarballPath             string             `json:"tarballPath,omitempty"`
	Tags                    []string           `json:"tags,omitempty"`
	SourceDateEpoch         time.Time          `json:"sourceDateEpoch,omitempty"`
	SBOMPath                string             `json:"sbomPath,omitempty"`
	SBOMFormats             []string           `json:"sbomFormats,omitempty"`
	ExtraKeyFiles           []string           `json:"extraKeyFiles,omitempty"`
	ExtraBuildRepos         []string           `json:"extraBuildRepos,omitempty"`
	ExtraRepos              []string           `json:"extraRepos,omitempty"`
	ExtraPackages           []string           `json:"extraPackages,omitempty"`
	Arch                    types.Architecture `json:"arch,omitempty"`
	TempDirPath             string             `json:"tempDirPath,omitempty"`
	PackageVersionTag       string             `json:"packageVersionTag,omitempty"`
	PackageVersionTagStem   bool               `json:"packageVersionTagStem,omitempty"`
	PackageVersionTagPrefix string             `json:"packageVersionTagPrefix,omitempty"`
	TagSuffix               string             `json:"tagSuffix,omitempty"`
	Local                   bool               `json:"local,omitempty"`
	CacheDir                string             `json:"cacheDir,omitempty"`
	Offline                 bool               `json:"offline,omitempty"`
	SharedCache             *apk.Cache         `json:"-"`
	Lockfile                string             `json:"lockfile,omitempty"`
	Auth                    auth.Authenticator `json:"-"`
	IncludePaths            []string           `json:"includePaths,omitempty"`
	IgnoreSignatures        bool               `json:"ignoreSignatures,omitempty"`
	Transport               http.RoundTripper  `json:"-"`

	// Concurrency controls for running apko as a service with many parallel builds.
	// These help prevent CPU and memory exhaustion when building multiple images concurrently.

	// GzipConcurrency controls the number of parallel threads used by pgzip for layer compression.
	// When set to 0 (default), uses min(GOMAXPROCS, 8) threads per compression operation.
	// For services running many parallel builds, set to 1-2 to prevent thread explosion.
	// Each gzip writer uses approximately (GzipConcurrency * 1MB) of memory for buffering.
	GzipConcurrency int `json:"gzipConcurrency,omitempty"`

	// APKFetchWorkers controls the number of parallel goroutines for fetching and expanding APK packages.
	// When set to 0 (default), uses GOMAXPROCS workers.
	// For services running many parallel builds, set to 4-8 to limit concurrent network/disk operations.
	APKFetchWorkers int `json:"apkFetchWorkers,omitempty"`

	// APKInstallWorkers controls the number of parallel goroutines for installing APK packages.
	// When set to 0 (default), uses GOMAXPROCS workers.
	// For memory-constrained environments, set to 1 for sequential installation.
	APKInstallWorkers int `json:"apkInstallWorkers,omitempty"`
}

type Auth struct{ User, Pass string }

var Default = Options{
	Arch:            types.ParseArchitecture(runtime.GOARCH),
	SourceDateEpoch: time.Unix(0, 0).UTC(),
	Auth:            auth.DefaultAuthenticators,
	SharedCache:     apk.NewCache(false),
}

// Tempdir returns the temporary directory where apko will create
// the layer blobs
func (o *Options) TempDir() string {
	if o.TempDirPath != "" {
		return o.TempDirPath
	}

	path, err := os.MkdirTemp(os.TempDir(), "apko-temp-*")
	if err != nil {
		log.Fatalf("creating tempdir: %v", err)
	}
	o.TempDirPath = path
	return o.TempDirPath
}

// TarballFileName returns a deterministic filename for the layer taball
func (o Options) TarballFileName() string {
	tarName := "apko.tar.gz"
	if o.Arch.String() != "" {
		tarName = fmt.Sprintf("apko-%s.tar.gz", o.Arch.ToAPK())
	}
	return tarName
}

// EffectiveGzipConcurrency returns the gzip concurrency to use.
// If GzipConcurrency is 0, returns min(GOMAXPROCS, 8) as the default.
func (o Options) EffectiveGzipConcurrency() int {
	if o.GzipConcurrency > 0 {
		return o.GzipConcurrency
	}
	return min(runtime.GOMAXPROCS(0), 8)
}

// EffectiveAPKFetchWorkers returns the number of APK fetch workers to use.
// If APKFetchWorkers is 0, returns GOMAXPROCS as the default.
func (o Options) EffectiveAPKFetchWorkers() int {
	if o.APKFetchWorkers > 0 {
		return o.APKFetchWorkers
	}
	return runtime.GOMAXPROCS(0)
}

// EffectiveAPKInstallWorkers returns the number of APK install workers to use.
// If APKInstallWorkers is 0, returns GOMAXPROCS as the default.
func (o Options) EffectiveAPKInstallWorkers() int {
	if o.APKInstallWorkers > 0 {
		return o.APKInstallWorkers
	}
	return runtime.GOMAXPROCS(0)
}
