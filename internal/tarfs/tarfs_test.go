// Copyright 2023 Chainguard, Inc.
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

package tarfs

import (
	"archive/tar"
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// createTestTar creates an in-memory tar archive with test files.
func createTestTar(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)

	files := []struct {
		name     string
		content  string
		mode     int64
		typeflag byte
		linkname string
	}{
		{name: "dir/", mode: 0755, typeflag: tar.TypeDir},
		{name: "dir/file1.txt", content: "hello world", mode: 0644, typeflag: tar.TypeReg},
		{name: "dir/file2.txt", content: "goodbye world", mode: 0644, typeflag: tar.TypeReg},
		{name: "dir/subdir/", mode: 0755, typeflag: tar.TypeDir},
		{name: "dir/subdir/nested.txt", content: "nested content", mode: 0644, typeflag: tar.TypeReg},
		{name: "link", mode: 0777, typeflag: tar.TypeSymlink, linkname: "dir/file1.txt"},
	}

	for _, f := range files {
		hdr := &tar.Header{
			Name:     f.name,
			Mode:     f.mode,
			Size:     int64(len(f.content)),
			Typeflag: f.typeflag,
			Linkname: f.linkname,
			ModTime:  time.Unix(1234567890, 0),
			Uid:      1000,
			Gid:      1000,
			Uname:    "testuser",
			Gname:    "testgroup",
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("failed to write tar header: %v", err)
		}
		if f.content != "" {
			if _, err := tw.Write([]byte(f.content)); err != nil {
				t.Fatalf("failed to write tar content: %v", err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("failed to close tar writer: %v", err)
	}

	return buf
}

func TestNew(t *testing.T) {
	tarData := createTestTar(t)
	fsys, err := New(bytes.NewReader(tarData.Bytes()), int64(tarData.Len()))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	// Verify we can read a file
	f, err := fsys.Open("dir/file1.txt")
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	content, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll() failed: %v", err)
	}
	if string(content) != "hello world" {
		t.Errorf("unexpected content: got %q, want %q", string(content), "hello world")
	}
}

func TestSaveAndLoadIndex(t *testing.T) {
	tarData := createTestTar(t)

	// Create the original FS by scanning the tar
	original, err := New(bytes.NewReader(tarData.Bytes()), int64(tarData.Len()))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	// Save the index to a temp file
	tmpDir := t.TempDir()
	indexPath := filepath.Join(tmpDir, "test.idx")
	if err := original.SaveIndex(indexPath); err != nil {
		t.Fatalf("SaveIndex() failed: %v", err)
	}

	// Verify the index file was created
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("index file not created: %v", err)
	}

	// Load from the cached index
	restored, err := NewFromIndex(bytes.NewReader(tarData.Bytes()), indexPath)
	if err != nil {
		t.Fatalf("NewFromIndex() failed: %v", err)
	}

	// Verify the restored FS has the same files
	if len(restored.files) != len(original.files) {
		t.Errorf("file count mismatch: got %d, want %d", len(restored.files), len(original.files))
	}

	// Verify we can read files from the restored FS
	testCases := []struct {
		path    string
		content string
	}{
		{"dir/file1.txt", "hello world"},
		{"dir/file2.txt", "goodbye world"},
		{"dir/subdir/nested.txt", "nested content"},
	}

	for _, tc := range testCases {
		f, err := restored.Open(tc.path)
		if err != nil {
			t.Errorf("Open(%q) failed: %v", tc.path, err)
			continue
		}
		content, err := io.ReadAll(f)
		if err != nil {
			t.Errorf("ReadAll(%q) failed: %v", tc.path, err)
			continue
		}
		if string(content) != tc.content {
			t.Errorf("content mismatch for %q: got %q, want %q", tc.path, string(content), tc.content)
		}
	}

	// Verify symlink works
	link, err := restored.Readlink("link")
	if err != nil {
		t.Errorf("Readlink() failed: %v", err)
	}
	if link != "dir/file1.txt" {
		t.Errorf("link target mismatch: got %q, want %q", link, "dir/file1.txt")
	}

	// Verify ReadDir works
	entries, err := restored.ReadDir("dir")
	if err != nil {
		t.Errorf("ReadDir() failed: %v", err)
	}
	if len(entries) != 3 { // file1.txt, file2.txt, subdir
		t.Errorf("ReadDir entries mismatch: got %d, want 3", len(entries))
	}
}

func TestNewFromIndexMissingFile(t *testing.T) {
	tarData := createTestTar(t)
	_, err := NewFromIndex(bytes.NewReader(tarData.Bytes()), "/nonexistent/path.idx")
	if err == nil {
		t.Error("NewFromIndex() should fail with missing index file")
	}
}

func TestNewFromIndexInvalidJSON(t *testing.T) {
	tarData := createTestTar(t)

	tmpDir := t.TempDir()
	indexPath := filepath.Join(tmpDir, "invalid.idx")
	if err := os.WriteFile(indexPath, []byte("not valid json"), 0644); err != nil {
		t.Fatalf("failed to write invalid index: %v", err)
	}

	_, err := NewFromIndex(bytes.NewReader(tarData.Bytes()), indexPath)
	if err == nil {
		t.Error("NewFromIndex() should fail with invalid JSON")
	}
}

func TestIndexPreservesMetadata(t *testing.T) {
	tarData := createTestTar(t)

	original, err := New(bytes.NewReader(tarData.Bytes()), int64(tarData.Len()))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	tmpDir := t.TempDir()
	indexPath := filepath.Join(tmpDir, "test.idx")
	if err := original.SaveIndex(indexPath); err != nil {
		t.Fatalf("SaveIndex() failed: %v", err)
	}

	restored, err := NewFromIndex(bytes.NewReader(tarData.Bytes()), indexPath)
	if err != nil {
		t.Fatalf("NewFromIndex() failed: %v", err)
	}

	// Verify metadata is preserved
	for i, origEntry := range original.files {
		restoredEntry := restored.files[i]

		if origEntry.Header.Name != restoredEntry.Header.Name {
			t.Errorf("name mismatch at %d: got %q, want %q", i, restoredEntry.Header.Name, origEntry.Header.Name)
		}
		if origEntry.Header.Size != restoredEntry.Header.Size {
			t.Errorf("size mismatch at %d: got %d, want %d", i, restoredEntry.Header.Size, origEntry.Header.Size)
		}
		if origEntry.Header.Mode != restoredEntry.Header.Mode {
			t.Errorf("mode mismatch at %d: got %d, want %d", i, restoredEntry.Header.Mode, origEntry.Header.Mode)
		}
		if origEntry.Header.Typeflag != restoredEntry.Header.Typeflag {
			t.Errorf("typeflag mismatch at %d: got %d, want %d", i, restoredEntry.Header.Typeflag, origEntry.Header.Typeflag)
		}
		if origEntry.Header.Linkname != restoredEntry.Header.Linkname {
			t.Errorf("linkname mismatch at %d: got %q, want %q", i, restoredEntry.Header.Linkname, origEntry.Header.Linkname)
		}
		if origEntry.Offset != restoredEntry.Offset {
			t.Errorf("offset mismatch at %d: got %d, want %d", i, restoredEntry.Offset, origEntry.Offset)
		}
		if origEntry.Header.Uid != restoredEntry.Header.Uid {
			t.Errorf("uid mismatch at %d: got %d, want %d", i, restoredEntry.Header.Uid, origEntry.Header.Uid)
		}
		if origEntry.Header.Gid != restoredEntry.Header.Gid {
			t.Errorf("gid mismatch at %d: got %d, want %d", i, restoredEntry.Header.Gid, origEntry.Header.Gid)
		}
	}
}

func TestStat(t *testing.T) {
	tarData := createTestTar(t)
	fsys, err := New(bytes.NewReader(tarData.Bytes()), int64(tarData.Len()))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	// Test file stat
	fi, err := fsys.Stat("dir/file1.txt")
	if err != nil {
		t.Fatalf("Stat() failed: %v", err)
	}
	if fi.Name() != "file1.txt" {
		t.Errorf("unexpected name: got %q, want %q", fi.Name(), "file1.txt")
	}
	if fi.Size() != 11 { // len("hello world")
		t.Errorf("unexpected size: got %d, want %d", fi.Size(), 11)
	}

	// Test root stat
	fi, err = fsys.Stat(".")
	if err != nil {
		t.Fatalf("Stat(\".\") failed: %v", err)
	}
	if !fi.IsDir() {
		t.Error("root should be a directory")
	}

	// Test non-existent file
	_, err = fsys.Stat("nonexistent")
	if err != fs.ErrNotExist {
		t.Errorf("expected ErrNotExist, got: %v", err)
	}
}

func BenchmarkNew(b *testing.B) {
	// Create a larger tar for benchmarking
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	for i := 0; i < 1000; i++ {
		hdr := &tar.Header{
			Name: "file" + string(rune(i)),
			Mode: 0644,
			Size: 100,
		}
		tw.WriteHeader(hdr)
		tw.Write(make([]byte, 100))
	}
	tw.Close()

	data := buf.Bytes()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		New(bytes.NewReader(data), int64(len(data)))
	}
}

func BenchmarkNewFromIndex(b *testing.B) {
	// Create a larger tar for benchmarking
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	for i := 0; i < 1000; i++ {
		hdr := &tar.Header{
			Name: "file" + string(rune(i)),
			Mode: 0644,
			Size: 100,
		}
		tw.WriteHeader(hdr)
		tw.Write(make([]byte, 100))
	}
	tw.Close()

	data := buf.Bytes()

	// Create the index
	fsys, _ := New(bytes.NewReader(data), int64(len(data)))
	tmpDir := b.TempDir()
	indexPath := filepath.Join(tmpDir, "bench.idx")
	fsys.SaveIndex(indexPath)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		NewFromIndex(bytes.NewReader(data), indexPath)
	}
}
