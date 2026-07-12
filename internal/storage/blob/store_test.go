package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestPutAllowsExactLimitAndOpenRoundTrips(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blobs")
	store := newTestStore(t, root)
	content := bytes.Repeat([]byte("openmessage"), 373)

	ref, err := store.Put(context.Background(), bytes.NewReader(content), "image/test", int64(len(content)))
	if err != nil {
		t.Fatalf("Put() at exact limit: %v", err)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256(content))
	if ref != (BlobRef{Hash: wantHash, Size: int64(len(content)), MIME: "image/test"}) {
		t.Fatalf("Put() ref = %+v, want hash=%q size=%d MIME=%q", ref, wantHash, len(content), "image/test")
	}

	reader, err := store.Open(ref)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		reader.Close()
		t.Fatalf("Read(): %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("Open() content differs: got %d bytes, want %d", len(got), len(content))
	}

	info, err := store.Stat(ref)
	if err != nil {
		t.Fatalf("Stat(): %v", err)
	}
	if info.Size() != int64(len(content)) {
		t.Fatalf("Stat().Size() = %d, want %d", info.Size(), len(content))
	}
	if gotMode := info.Mode().Perm(); gotMode != privateFileMode {
		t.Fatalf("blob mode = %04o, want %04o", gotMode, privateFileMode)
	}
}

func TestPutRejectsLimitPlusOneAndCleansUp(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blobs")
	store := newTestStore(t, root)
	const max = 4096
	reader := &countingReader{data: bytes.Repeat([]byte{0x7a}, max+1000)}

	_, err := store.Put(context.Background(), reader, "application/octet-stream", max)
	var tooLarge *ErrTooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("Put() error = %v, want *ErrTooLarge", err)
	}
	if tooLarge.Max != max {
		t.Fatalf("ErrTooLarge.Max = %d, want %d", tooLarge.Max, max)
	}
	if reader.bytesRead != max+1 {
		t.Fatalf("Put() read %d bytes, want exactly max+1 (%d)", reader.bytesRead, max+1)
	}
	if files := regularFiles(t, root); len(files) != 0 {
		t.Fatalf("oversize Put() left files: %v", files)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir(root): %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("oversize Put() left root entries: %v", entryNames(entries))
	}
}

func TestPutClassifiesLimitPlusOneBeforeReaderError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blobs")
	store := newTestStore(t, root)
	sourceErr := errors.New("source failed")
	reader := readerFunc(func(p []byte) (int, error) {
		for i := range p {
			p[i] = byte(i)
		}
		return len(p), sourceErr
	})

	_, err := store.Put(context.Background(), reader, "", 8)
	var tooLarge *ErrTooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("Put() error = %v, want *ErrTooLarge", err)
	}
	if errors.Is(err, sourceErr) {
		t.Fatalf("Put() returned source error instead of size error: %v", err)
	}
	if files := regularFiles(t, root); len(files) != 0 {
		t.Fatalf("failed Put() left files: %v", files)
	}
}

func TestPutDeduplicatesAndUsesPrivateModes(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "blobs")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("Mkdir(root): %v", err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("Chmod(root): %v", err)
	}
	store := newTestStore(t, root)
	content := bytes.Repeat([]byte("deduplicate me"), 128)

	first, err := store.Put(context.Background(), bytes.NewReader(content), "image/png", int64(len(content)))
	if err != nil {
		t.Fatalf("first Put(): %v", err)
	}
	second, err := store.Put(context.Background(), bytes.NewReader(content), "image/png", int64(len(content)))
	if err != nil {
		t.Fatalf("second Put(): %v", err)
	}
	if first != second {
		t.Fatalf("duplicate refs differ: first=%+v second=%+v", first, second)
	}

	files := regularFiles(t, root)
	if len(files) != 1 {
		t.Fatalf("duplicate Put() left %d regular files (%v), want 1", len(files), files)
	}
	wantPath := filepath.Join(root, first.Hash[:2], first.Hash[2:])
	if files[0] != wantPath {
		t.Fatalf("blob path = %q, want %q", files[0], wantPath)
	}
	assertMode(t, root, privateDirMode)
	assertMode(t, filepath.Dir(wantPath), privateDirMode)
	assertMode(t, wantPath, privateFileMode)
}

func TestConcurrentIdenticalPutsAcrossStoresLeaveOneFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blobs")
	content := bytes.Repeat([]byte("concurrent content"), 1024)

	const puts = 16
	stores := make([]*BlobStore, puts)
	for i := range stores {
		stores[i] = newTestStore(t, root)
	}
	refs := make([]BlobRef, puts)
	errs := make([]error, puts)
	var wg sync.WaitGroup
	for i := range puts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			refs[i], errs[i] = stores[i].Put(
				context.Background(),
				bytes.NewReader(content),
				"video/test",
				int64(len(content)),
			)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Put()[%d]: %v", i, err)
		}
		if refs[i] != refs[0] {
			t.Fatalf("Put()[%d] ref = %+v, want %+v", i, refs[i], refs[0])
		}
	}
	if files := regularFiles(t, root); len(files) != 1 {
		t.Fatalf("concurrent Put() left %d regular files (%v), want 1", len(files), files)
	}
}

func TestOpenRejectsSymlinkAtHashPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blobs")
	store := newTestStore(t, root)
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("outside the blob root"), privateFileMode); err != nil {
		t.Fatalf("WriteFile(target): %v", err)
	}

	hash := strings.Repeat("a", sha256.Size*2)
	shard := filepath.Join(root, hash[:2])
	if err := os.MkdirAll(shard, privateDirMode); err != nil {
		t.Fatalf("MkdirAll(shard): %v", err)
	}
	if err := os.Symlink(target, filepath.Join(shard, hash[2:])); err != nil {
		t.Skipf("Symlink() unavailable: %v", err)
	}

	if _, err := store.Open(BlobRef{Hash: hash}); err == nil {
		t.Fatal("Open() followed a symlink at the hash path")
	}
}

func TestDeleteRemovesBlob(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blobs")
	store := newTestStore(t, root)
	ref, err := store.Put(context.Background(), bytes.NewBufferString("delete me"), "text/plain", 9)
	if err != nil {
		t.Fatalf("Put(): %v", err)
	}

	if err := store.Delete(ref); err != nil {
		t.Fatalf("Delete(): %v", err)
	}
	if _, err := store.Stat(ref); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat() after Delete() error = %v, want os.ErrNotExist", err)
	}
	if _, err := store.Open(ref); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open() after Delete() error = %v, want os.ErrNotExist", err)
	}
	if files := regularFiles(t, root); len(files) != 0 {
		t.Fatalf("Delete() left files: %v", files)
	}
}

func TestPutEmptyAtZeroLimitAndMaxIntLimit(t *testing.T) {
	for _, max := range []int64{0, math.MaxInt64} {
		t.Run(fmt.Sprintf("max=%d", max), func(t *testing.T) {
			store := newTestStore(t, filepath.Join(t.TempDir(), "blobs"))
			ref, err := store.Put(context.Background(), bytes.NewReader(nil), "", max)
			if err != nil {
				t.Fatalf("Put(empty): %v", err)
			}
			if ref.Size != 0 {
				t.Fatalf("Put(empty).Size = %d, want 0", ref.Size)
			}
		})
	}
}

func TestBlobOperationsRejectNonCanonicalHash(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "blobs"))
	invalid := []string{
		"",
		"../" + string(bytes.Repeat([]byte{'a'}, 61)),
		string(bytes.Repeat([]byte{'A'}, 64)),
		string(bytes.Repeat([]byte{'g'}, 64)),
	}
	for _, hash := range invalid {
		ref := BlobRef{Hash: hash}
		if _, err := store.Open(ref); err == nil {
			t.Errorf("Open(%q) succeeded", hash)
		}
		if _, err := store.Stat(ref); err == nil {
			t.Errorf("Stat(%q) succeeded", hash)
		}
		if err := store.Delete(ref); err == nil {
			t.Errorf("Delete(%q) succeeded", hash)
		}
	}
}

type countingReader struct {
	data      []byte
	bytesRead int
}

func (r *countingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	r.bytesRead += n
	return n, nil
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) {
	return f(p)
}

func newTestStore(t *testing.T, root string) *BlobStore {
	t.Helper()
	store, err := New(root)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return store
}

func regularFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%q): %v", root, err)
	}
	return files
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q): %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode(%q) = %04o, want %04o", path, got, want)
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
