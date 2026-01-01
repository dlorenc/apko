package types_test

import (
	"context"
	"crypto/sha256"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"chainguard.dev/apko/pkg/build/types"
)

var (
	gid1000  = uint32(1000)
	gid1001  = uint32(1001)
	gid1000T = types.GID(&gid1000)
	gid1001T = types.GID(&gid1001)
)

func TestOverlayWithEmptyContents(t *testing.T) {
	ctx := context.Background()

	configPath := filepath.Join("overlay", "overlay.apko.yaml")
	hasher := sha256.New()
	ic := types.ImageConfiguration{}

	require.NoError(t, ic.Load(ctx, configPath, []string{"testdata"}, hasher))
	require.ElementsMatch(t, ic.Contents.BuildRepositories, []string{"secret repository"})
	require.ElementsMatch(t, ic.Contents.RuntimeOnlyRepositories, []string{"runtime repository"})
	require.ElementsMatch(t, ic.Contents.Repositories, []string{"repository"})
	require.ElementsMatch(t, ic.Contents.Keyring, []string{"key"})
	require.ElementsMatch(t, ic.Contents.Packages, []string{"package"})
}

func TestOverlayWithAdditionalPackages(t *testing.T) {
	ctx := context.Background()

	configPath := filepath.Join("testdata", "overlay", "overlay_with_package.apko.yaml")
	hasher := sha256.New()
	ic := types.ImageConfiguration{}

	require.NoError(t, ic.Load(ctx, configPath, []string{}, hasher))
	require.ElementsMatch(t, ic.Contents.BuildRepositories, []string{"secret repository", "other_secret repository"})
	require.ElementsMatch(t, ic.Contents.RuntimeOnlyRepositories, []string{"runtime repository", "other runtime repository"})
	require.ElementsMatch(t, ic.Contents.Repositories, []string{"repository"})
	require.ElementsMatch(t, ic.Contents.Keyring, []string{"key"})
	require.ElementsMatch(t, ic.Contents.Packages, []string{"package", "other_package"})
}

func TestUserContents(t *testing.T) {
	ctx := context.Background()

	configPath := filepath.Join("testdata", "users.apko.yaml")
	hasher := sha256.New()
	ic := types.ImageConfiguration{}

	require.NoError(t, ic.Load(ctx, configPath, []string{}, hasher))
	if err := ic.Validate(); err != nil {
		t.Fatal(err)
	}

	require.Equal(t, "/not/home", ic.Accounts.Users[0].HomeDir)
	require.Equal(t, "/home/user", ic.Accounts.Users[1].HomeDir)

	// Ensure this does not cause panic when users[1].gid is empty (defaulting to 0)
	ic.Summarize(ctx)
}

func TestMergeInto(t *testing.T) {
	tests := []struct {
		name     string
		source   types.ImageConfiguration
		target   types.ImageConfiguration
		expected types.ImageConfiguration
	}{{
		name: "simple blend of contents",
		source: types.ImageConfiguration{
			Contents: types.ImageContents{
				Keyring:           []string{"foo"},
				BuildRepositories: []string{"foo"},
				Repositories:      []string{"foo"},
				Packages:          []string{"foo"},
			},
			Environment: map[string]string{
				"EXTRA": "foo",
				"VAR":   "foo",
			},
			Annotations: map[string]string{
				"org.extra": "foo",
				"org.blah":  "foo",
			},
			Volumes: []string{
				"volume1",
			},
			Certificates: &types.ImageCertificates{
				Additional: []types.AdditionalCertificateEntry{{
					Name:    "foo",
					Content: "bar",
				}},
			},
		},
		target: types.ImageConfiguration{
			Contents: types.ImageContents{
				Keyring:           []string{"bar"},
				BuildRepositories: []string{"bar"},
				Repositories:      []string{"bar"},
				Packages:          []string{"bar"},
			},
		},
		expected: types.ImageConfiguration{
			Contents: types.ImageContents{
				Keyring:           []string{"foo", "bar"},
				BuildRepositories: []string{"foo", "bar"},
				Repositories:      []string{"foo", "bar"},
				Packages:          []string{"foo", "bar"},
			},
			Environment: map[string]string{
				"EXTRA": "foo",
				"VAR":   "foo",
			},
			Annotations: map[string]string{
				"org.extra": "foo",
				"org.blah":  "foo",
			},
			Volumes: []string{
				"volume1",
			},
			Certificates: &types.ImageCertificates{
				Additional: []types.AdditionalCertificateEntry{{
					Name:    "foo",
					Content: "bar",
				}},
			},
		},
	}, {
		name: "simple blend of contents",
		source: types.ImageConfiguration{
			Contents: types.ImageContents{
				Keyring:           []string{"foo"},
				BuildRepositories: []string{"foo"},
				Repositories:      []string{"foo"},
				Packages:          []string{"foo"},
			},
		},
		target: types.ImageConfiguration{
			Contents: types.ImageContents{
				Keyring:           []string{"bar"},
				BuildRepositories: []string{"bar"},
				Repositories:      []string{"bar"},
				Packages:          []string{"bar"},
			},
		},
		expected: types.ImageConfiguration{
			Contents: types.ImageContents{
				Keyring:           []string{"foo", "bar"},
				BuildRepositories: []string{"foo", "bar"},
				Repositories:      []string{"foo", "bar"},
				Packages:          []string{"foo", "bar"},
			},
		},
	}, {
		name: "conflict resolution",
		source: types.ImageConfiguration{
			Contents: types.ImageContents{
				Keyring:           []string{"foo"},
				BuildRepositories: []string{"foo"},
				Repositories:      []string{"foo"},
				Packages:          []string{"foo"},
			},
			Cmd:        "foo",
			StopSignal: "foo",
			WorkDir:    "foo",
			Accounts: types.ImageAccounts{
				RunAs: "foo",
				Users: []types.User{{
					UserName: "foo",
					UID:      1000,
					GID:      gid1000T,
					HomeDir:  "/home/foo",
				}},
			},
			Environment: map[string]string{
				"EXTRA": "foo",
				"VAR":   "foo",
			},
			Annotations: map[string]string{
				"org.extra": "foo",
				"org.blah":  "foo",
			},
		},
		target: types.ImageConfiguration{
			Cmd:        "bar",
			StopSignal: "bar",
			WorkDir:    "bar",
			Accounts: types.ImageAccounts{
				RunAs: "bar",
				Users: []types.User{{
					UserName: "bar",
					UID:      1001,
					GID:      gid1001T,
					HomeDir:  "/home/bar",
				}},
			},
			Environment: map[string]string{
				"VAR": "bar",
			},
			Annotations: map[string]string{
				"org.blah": "bar",
			},
		},
		expected: types.ImageConfiguration{
			Contents: types.ImageContents{
				Keyring:           []string{"foo"},
				BuildRepositories: []string{"foo"},
				Repositories:      []string{"foo"},
				Packages:          []string{"foo"},
			},
			Cmd:        "bar",
			StopSignal: "bar",
			WorkDir:    "bar",
			Accounts: types.ImageAccounts{
				RunAs: "bar",
				Users: []types.User{{
					UserName: "foo",
					UID:      1000,
					GID:      gid1000T,
					HomeDir:  "/home/foo",
				}, {
					UserName: "bar",
					UID:      1001,
					GID:      gid1001T,
					HomeDir:  "/home/bar",
				}},
			},
			Environment: map[string]string{
				"EXTRA": "foo",
				"VAR":   "bar",
			},
			Annotations: map[string]string{
				"org.extra": "foo",
				"org.blah":  "bar",
			},
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.source.MergeInto(&tt.target)
			require.NoError(t, err)
			require.Equal(t, tt.expected, tt.target)
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name          string
		configuration types.ImageConfiguration
		expectError   string
	}{{
		name: "no cert name",
		configuration: types.ImageConfiguration{
			Certificates: &types.ImageCertificates{
				Additional: []types.AdditionalCertificateEntry{{
					Name:    "",
					Content: "test",
				}},
			},
		},
		expectError: "configured additional certificate has no name",
	}, {
		name: "path walking cert name",
		configuration: types.ImageConfiguration{
			Certificates: &types.ImageCertificates{
				Additional: []types.AdditionalCertificateEntry{{
					Name:    "trying/../../../to/break",
					Content: "test",
				}},
			},
		},
		expectError: `configured additional certificate "trying/../../../to/break" has an invalid name, it must match ^[a-zA-Z0-9_-]+$`,
	}, {
		name: "weird characters cert name",
		configuration: types.ImageConfiguration{
			Certificates: &types.ImageCertificates{
				Additional: []types.AdditionalCertificateEntry{{
					Name:    "my-cert@123!",
					Content: "test",
				}},
			},
		},
		expectError: `configured additional certificate "my-cert@123!" has an invalid name, it must match ^[a-zA-Z0-9_-]+$`,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.configuration.Validate()
			require.EqualError(t, err, tt.expectError)
		})
	}
}

func TestImageConfiguration_Hash(t *testing.T) {
	// Test that the hash is deterministic
	ic := types.ImageConfiguration{
		Contents: types.ImageContents{
			Packages:     []string{"pkg1", "pkg2", "pkg3"},
			Repositories: []string{"repo1", "repo2"},
			Keyring:      []string{"key1"},
		},
		Entrypoint: types.ImageEntrypoint{
			Command: "/bin/sh",
		},
		Environment: map[string]string{
			"VAR1": "value1",
			"VAR2": "value2",
		},
	}

	arch := types.Architecture("amd64")

	// Compute hash multiple times - should be identical
	hash1 := ic.Hash(arch)
	hash2 := ic.Hash(arch)
	hash3 := ic.Hash(arch)

	require.Equal(t, hash1, hash2, "hash should be deterministic")
	require.Equal(t, hash2, hash3, "hash should be deterministic")
	require.Len(t, hash1, 64, "hash should be 64 hex characters (sha256)")
}

func TestImageConfiguration_Hash_DifferentArch(t *testing.T) {
	ic := types.ImageConfiguration{
		Contents: types.ImageContents{
			Packages: []string{"pkg1"},
		},
	}

	hashAmd64 := ic.Hash(types.Architecture("amd64"))
	hashArm64 := ic.Hash(types.Architecture("arm64"))

	require.NotEqual(t, hashAmd64, hashArm64, "different architectures should produce different hashes")
}

func TestImageConfiguration_Hash_DifferentPackages(t *testing.T) {
	ic1 := types.ImageConfiguration{
		Contents: types.ImageContents{
			Packages: []string{"pkg1", "pkg2"},
		},
	}

	ic2 := types.ImageConfiguration{
		Contents: types.ImageContents{
			Packages: []string{"pkg1", "pkg3"},
		},
	}

	arch := types.Architecture("amd64")

	require.NotEqual(t, ic1.Hash(arch), ic2.Hash(arch), "different packages should produce different hashes")
}

func TestImageConfiguration_Hash_PackageOrderIndependent(t *testing.T) {
	// Package order shouldn't matter since we sort before hashing
	ic1 := types.ImageConfiguration{
		Contents: types.ImageContents{
			Packages: []string{"pkg1", "pkg2", "pkg3"},
		},
	}

	ic2 := types.ImageConfiguration{
		Contents: types.ImageContents{
			Packages: []string{"pkg3", "pkg1", "pkg2"},
		},
	}

	arch := types.Architecture("amd64")

	require.Equal(t, ic1.Hash(arch), ic2.Hash(arch), "package order should not affect hash")
}

func TestImageConfiguration_Hash_EnvironmentOrderIndependent(t *testing.T) {
	ic1 := types.ImageConfiguration{
		Environment: map[string]string{
			"A": "1",
			"B": "2",
			"C": "3",
		},
	}

	// Go maps have non-deterministic iteration order, but hash should be consistent
	arch := types.Architecture("amd64")

	hash1 := ic1.Hash(arch)
	hash2 := ic1.Hash(arch)

	require.Equal(t, hash1, hash2, "environment hash should be deterministic despite map iteration order")
}

func TestImageConfiguration_Hash_DifferentEnvironment(t *testing.T) {
	ic1 := types.ImageConfiguration{
		Environment: map[string]string{
			"VAR": "value1",
		},
	}

	ic2 := types.ImageConfiguration{
		Environment: map[string]string{
			"VAR": "value2",
		},
	}

	arch := types.Architecture("amd64")

	require.NotEqual(t, ic1.Hash(arch), ic2.Hash(arch), "different environment values should produce different hashes")
}

func TestImageConfiguration_Hash_WithLayering(t *testing.T) {
	ic1 := types.ImageConfiguration{
		Contents: types.ImageContents{
			Packages: []string{"pkg1"},
		},
		Layering: &types.Layering{
			Strategy: "origin",
			Budget:   10,
		},
	}

	ic2 := types.ImageConfiguration{
		Contents: types.ImageContents{
			Packages: []string{"pkg1"},
		},
		Layering: &types.Layering{
			Strategy: "origin",
			Budget:   20,
		},
	}

	ic3 := types.ImageConfiguration{
		Contents: types.ImageContents{
			Packages: []string{"pkg1"},
		},
		// No layering
	}

	arch := types.Architecture("amd64")

	require.NotEqual(t, ic1.Hash(arch), ic2.Hash(arch), "different layering budget should produce different hashes")
	require.NotEqual(t, ic1.Hash(arch), ic3.Hash(arch), "layering vs no layering should produce different hashes")
}

func TestImageConfiguration_Hash_WithUsers(t *testing.T) {
	ic1 := types.ImageConfiguration{
		Accounts: types.ImageAccounts{
			Users: []types.User{{
				UserName: "user1",
				UID:      1000,
			}},
		},
	}

	ic2 := types.ImageConfiguration{
		Accounts: types.ImageAccounts{
			Users: []types.User{{
				UserName: "user2",
				UID:      1000,
			}},
		},
	}

	arch := types.Architecture("amd64")

	require.NotEqual(t, ic1.Hash(arch), ic2.Hash(arch), "different users should produce different hashes")
}
