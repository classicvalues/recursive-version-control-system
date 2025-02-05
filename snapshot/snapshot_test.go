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
	"time"
)

// storageForTest defines persistent storage of snapshots.
type storageForTest struct {
	mu           sync.Mutex
	objects      map[Hash][]byte
	snapshots    map[Path]*Hash
	cache        map[Path]os.FileInfo
	cacheModTime map[Path]time.Time
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
	if s.cacheModTime == nil {
		s.cacheModTime = make(map[Path]time.Time)
	}
	s.cache[p] = info
	s.cacheModTime[p] = info.ModTime()
	return nil
}

// PathInfoMatchesCache reports whether or not the given file
// information matches the file information that was previously cached
// for the given path.
func (s *storageForTest) PathInfoMatchesCache(ctx context.Context, p Path, info os.FileInfo) bool {
	if s == nil {
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
	cachedModTime, ok := s.cacheModTime[p]
	if !ok {
		return false
	}
	return os.SameFile(cached, info) && cachedModTime.Equal(info.ModTime())
}

// timeNowMutex is a mutex to make sure that no two test runs try to modify the `timeNow` package variable simultaneously.
//
// Our unit tests sometimes modify the `timeNow` package variable for testing purposes.
//
// This means that if the test runs multiple times in parallel, then they can stomp
// on each other causing spurious test failures. To prevent that we first aqcuire this lock.
var timeNowMutex sync.Mutex

func TestCurrentSingleFile(t *testing.T) {
	timeNowMutex.Lock()
	defer timeNowMutex.Unlock()
	defer func() { timeNow = time.Now }()
	dir := t.TempDir()
	file := filepath.Join(dir, "example.txt")
	p := Path(file)
	s := &storageForTest{}

	// Take an initial snapshot
	if err := os.WriteFile(file, []byte("Hello, World!"), 0700); err != nil {
		t.Fatalf("failure creating the example file to snapshot: %v", err)
	}
	h1, f1, err := Current(context.Background(), s, p)
	if err != nil {
		t.Errorf("failure creating the initial snapshot for the file: %v", err)
	} else if h1 == nil {
		t.Error("unexpected nil hash for the file")
	} else if f1 == nil {
		t.Error("unexpected nil snapshot for the file")
	}

	// Verify that we can take the snapshot again without it changing
	h2, f2, err := Current(context.Background(), s, p)
	if err != nil {
		t.Errorf("failure replicating the initial snapshot for the file: %v", err)
	} else if got, want := h2, h1; !got.Equal(want) {
		t.Errorf("unexpected hash for the file; got %q, want %q", got, want)
	} else if got, want := f2.String(), f1.String(); got != want {
		t.Errorf("unexpected snapshot for the file; got %q, want %q", got, want)
	}

	// Modify the file and verify that the snapshot both changes and points to the parent
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

	// Force the snapshot to be cached
	prevInfo, err := os.Stat(file)
	if err != nil {
		t.Fatalf("failure reading the file info for our test file: %v", err)
	}
	timeNow = func() time.Time { return prevInfo.ModTime().Truncate(time.Second).Add(time.Second) }
	h4, _, err := Current(context.Background(), s, p)
	if err != nil {
		t.Errorf("failure creating the updated snapshot for the file: %v", err)
	} else if got, want := h4, h3; !got.Equal(want) {
		t.Errorf("unexpected hash for unchanged file: got %q, want %q", got, want)
	}

	// Verify that we can read from the cache
	h5, _, err := Current(context.Background(), s, p)
	if err != nil {
		t.Errorf("failure reading the cached snapshot for the file: %v", err)
	} else if got, want := h5, h3; !got.Equal(want) {
		t.Errorf("unexpected hash for unchanged file: got %q, want %q", got, want)
	}

	// Update the file to make sure the cache is bypassed
	// We have to make sure that the updated mod time is after the cached mod time.
	// These file times are (on Unix systems) seconds, so we have to wait for one
	// second more.
	prevInfo, err = os.Stat(file)
	if err != nil {
		t.Fatalf("failure reading the file info for our test file: %v", err)
	}
	firstValidTime := prevInfo.ModTime().Truncate(time.Second).Add(time.Second)
	currTime := time.Now()
	if firstValidTime.After(currTime) {
		time.Sleep(firstValidTime.Sub(currTime))
	}
	if err := os.WriteFile(file, []byte("I'm back, World!"), 0700); err != nil {
		t.Fatalf("failure updating the example file to snapshot: %v", err)
	}
	h6, f6, err := Current(context.Background(), s, p)
	if err != nil {
		t.Errorf("failure updating the cached snapshot for the file: %v", err)
	} else if h6.Equal(h5) {
		t.Error("failed to update the cached snapshot")
	} else if f6 == nil {
		t.Error("failed to read the update to the cached snapshot")
	} else if len(f6.Parents) == 0 || !f6.Parents[0].Equal(h5) {
		t.Error("failed to set the cached snapshot as the first parent of the update to the cached snapshot")
	}
}
