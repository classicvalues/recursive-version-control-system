// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package snapshot

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// storageForTest defines persistent storage of snapshots.
type storageForTest struct {
	mu           sync.Mutex
	objects      map[Hash][]byte
	snapshots    map[Path]*Hash
	enableCache  bool
	cache        map[Path]os.FileInfo
}

// StoreObject persists the contents of the given reader, returning the resulting hash of those contents.
//
// This is used for persistently storing the contents of individual files.
func (s *storageForTest) StoreObject(ctx context.Context, reader io.Reader) (*Hash, error) {
	if s == nil {
		return nil, fmt.Errorf("storage is not set")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objects == nil {
		s.objects = make(map[Hash][]byte)
	}
	var buf bytes.Buffer
	reader = io.TeeReader(reader, &buf)
	h, err := NewHash(reader)
	if err != nil {
		return nil, err
	}
	s.objects[*h] = buf.Bytes()
	return h, nil
}

// Exclude reports whether or not the given path should be excluded from storage.
func (s *storageForTest) Exclude(Path) bool { return false }

// FindSnapshot reads the latest snapshot (if any) for the given path.
func (s *storageForTest) FindSnapshot(ctx context.Context, p Path) (*Hash, *File, error) {
	if s == nil {
		return nil, nil, fmt.Errorf("storage is not set")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshots == nil || s.objects == nil {
		return nil, nil, nil
	}
	h, ok := s.snapshots[p]
	if !ok {
		return nil, nil, nil
	}
	bs, ok := s.objects[*h]
	if !ok {
		return nil, nil, fmt.Errorf("failure reading the contents of %q", h)
	}
	f, err := ParseFile(string(bs))
	if err != nil {
		return nil, nil, fmt.Errorf("failure parsing the previously saved snapshot %q: %v", string(bs), err)
	}
	return h, f, nil
}

// StoreSnapshot stores a mapping from the given path to the given snapshot.
func (s *storageForTest) StoreSnapshot(ctx context.Context, p Path, f *File) (*Hash, error) {
	if s == nil {
		return nil, fmt.Errorf("storage is not set")
	}
	h, err := s.StoreObject(ctx, strings.NewReader(f.String()))
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshots == nil {
		s.snapshots = make(map[Path]*Hash)
	}
	s.snapshots[p] = h
	return h, nil
}

// CachePathInfo caches the file information for the given path.
//
// This is used to avoid rehashing the contents of files that have
// not changed since the last time they were snapshotted.
func (s *storageForTest) CachePathInfo(ctx context.Context, p Path, info os.FileInfo) error {
	if s == nil {
		return fmt.Errorf("storage is not set")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		s.cache = make(map[Path]os.FileInfo)
	}
	s.cache[p] = info
	return nil
}

// PathInfoMatchesCache reports whether or not the given file
// information matches the file information that was previously cached
// for the given path.
func (s *storageForTest) PathInfoMatchesCache(ctx context.Context, p Path, info os.FileInfo) bool {
	if s == nil || !s.enableCache {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		return false
	}
	cached, ok := s.cache[p]
	if !ok {
		return false
	}
	return os.SameFile(cached, info)
}

func TestCurrentSingleFile(t *testing.T) {
	dir, err := os.MkdirTemp("", "snapshotTesting")
	if err != nil {
		t.Fatalf("failure creating the temporary testing directory: %v", err)
	}
	defer os.RemoveAll(dir)

	file := filepath.Join(dir, "example.txt")
	if err := os.WriteFile(file, []byte("Hello, World!"), 0700); err != nil {
		t.Fatalf("failure creating the example file to snapshot: %v", err)
	}
	p := Path(file)
	s := &storageForTest{}
	h1, f1, err := Current(context.Background(), s, p)
	if err != nil {
		t.Errorf("failure creating the initial snapshot for the file: %v", err)
	} else if h1 == nil {
		t.Error("unexpected nil hash for the file")
	} else if f1 == nil {
		t.Error("unexpected nil snapshot for the file")
	}
	h2, f2, err := Current(context.Background(), s, p)
	if err != nil {
		t.Errorf("failure replicating the initial snapshot for the file: %v", err)
	} else if got, want := h2, h1; !got.Equal(want) {
		t.Errorf("unexpected hash for the file; got %q, want %q", got, want)
	} else if got, want := f2.String(), f1.String(); got != want {
		t.Errorf("unexpected snapshot for the file; got %q, want %q", got, want)
	}

	if err := os.WriteFile(file, []byte("Goodbye, World!"), 0700); err != nil {
		t.Fatalf("failure updating the example file to snapshot: %v", err)
	}
	h3, f3, err := Current(context.Background(), s, p)
	if err != nil {
		t.Errorf("failure creating the updated snapshot for the file: %v", err)
	} else if h3 == nil {
		t.Error("unexpected nil hash for the updated file")
	} else if f3 == nil {
		t.Error("unexpected nil snapshot for the updated file")
	} else if h3.Equal(h1) {
		t.Error("failed to update the snapshot")
	} else if !f3.Parents[0].Equal(h1) {
		t.Errorf("updated snapshot did not include the original as its parent: %q", f3)
	}
}

func TestCurrentSingleFileWithCacheCheck(t *testing.T) {
	dir, err := os.MkdirTemp("", "snapshotTesting")
	if err != nil {
		t.Fatalf("failure creating the temporary testing directory: %v", err)
	}
	defer os.RemoveAll(dir)

	file := filepath.Join(dir, "example.txt")
	if err := os.WriteFile(file, []byte("Hello, World!"), 0700); err != nil {
		t.Fatalf("failure creating the example file to snapshot: %v", err)
	}
	p := Path(file)
	s := &storageForTest{enableCache: true}
	h1, f1, err := Current(context.Background(), s, p)
	if err != nil {
		t.Errorf("failure creating the initial snapshot for the file: %v", err)
	} else if h1 == nil {
		t.Error("unexpected nil hash for the file")
	} else if f1 == nil {
		t.Error("unexpected nil snapshot for the file")
	}

	if err := os.WriteFile(file, []byte("Goodbye, World!"), 0700); err != nil {
		t.Fatalf("failure updating the example file to snapshot: %v", err)
	}
	h2, f2, err := Current(context.Background(), s, p)
	if err != nil {
		t.Errorf("failure replicating the initial snapshot for the file: %v", err)
	} else if got, want := h2, h1; !got.Equal(want) {
		t.Errorf("unexpected hash for the file; got %q, want %q", got, want)
	} else if got, want := f2.String(), f1.String(); got != want {
		t.Errorf("unexpected snapshot for the file; got %q, want %q", got, want)
	}

	s.enableCache = false
	h3, f3, err := Current(context.Background(), s, p)
	if err != nil {
		t.Errorf("failure creating the updated snapshot for the file: %v", err)
	} else if h3 == nil {
		t.Error("unexpected nil hash for the updated file")
	} else if f3 == nil {
		t.Error("unexpected nil snapshot for the updated file")
	} else if h3.Equal(h1) {
		t.Error("failed to update the snapshot")
	} else if !f3.Parents[0].Equal(h1) {
		t.Errorf("updated snapshot did not include the original as its parent: %q", f3)
	}
}
