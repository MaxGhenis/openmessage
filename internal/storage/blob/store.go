// Package blob provides private, content-addressed storage for media blobs.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	privateDirMode  = 0o700
	privateFileMode = 0o600
	copyBufferSize  = 32 * 1024
)

// BlobRef identifies a blob and records the metadata supplied when it was put.
type BlobRef struct {
	Hash string
	Size int64
	MIME string
}

// ErrTooLarge reports that a reader contained more than the configured hard
// byte cap. Put consumes at most Max+1 bytes before returning this error.
type ErrTooLarge struct {
	Max int64
}

func (e *ErrTooLarge) Error() string {
	return fmt.Sprintf("blob exceeds maximum size of %d bytes", e.Max)
}

// BlobStore stores blobs beneath a private root directory. Blob contents are
// addressed by lowercase SHA-256 hashes and sharded as root/ab/cdef....
type BlobStore struct {
	root string
	mu   sync.RWMutex
}

// New creates a BlobStore rooted at root. Existing roots are tightened to
// mode 0700; a symlink or non-directory root is rejected.
func New(root string) (*BlobStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("create blob store: root is empty")
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("create blob store: resolve root: %w", err)
	}
	if err := ensurePrivateDir(absRoot); err != nil {
		return nil, fmt.Errorf("create blob store: %w", err)
	}

	return &BlobStore{root: absRoot}, nil
}

// Put streams r into the store while hashing it. It reads no more than max+1
// bytes, never buffers the complete blob, and atomically deduplicates by hash.
func (s *BlobStore) Put(ctx context.Context, r io.Reader, declaredMIME string, max int64) (BlobRef, error) {
	if s == nil {
		return BlobRef{}, fmt.Errorf("put blob: store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil {
		return BlobRef{}, fmt.Errorf("put blob: context is nil")
	}
	if r == nil {
		return BlobRef{}, fmt.Errorf("put blob: reader is nil")
	}
	if max < 0 {
		return BlobRef{}, fmt.Errorf("put blob: max must be non-negative")
	}
	if err := ctx.Err(); err != nil {
		return BlobRef{}, fmt.Errorf("put blob: %w", err)
	}
	if err := ensurePrivateDir(s.root); err != nil {
		return BlobRef{}, fmt.Errorf("put blob: %w", err)
	}

	tmp, err := os.CreateTemp(s.root, ".blob-put-*")
	if err != nil {
		return BlobRef{}, fmt.Errorf("put blob: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	stageDir := ""
	defer func() {
		_ = tmp.Close()
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
		if stageDir != "" {
			_ = os.Remove(stageDir)
		}
	}()

	if err := tmp.Chmod(privateFileMode); err != nil {
		return BlobRef{}, fmt.Errorf("put blob: set temp file mode: %w", err)
	}

	hasher := sha256.New()
	size, err := copyCapped(ctx, io.MultiWriter(tmp, hasher), r, max)
	if err != nil {
		var tooLarge *ErrTooLarge
		if errors.As(err, &tooLarge) {
			return BlobRef{}, tooLarge
		}
		return BlobRef{}, fmt.Errorf("put blob: stream content: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return BlobRef{}, fmt.Errorf("put blob: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return BlobRef{}, fmt.Errorf("put blob: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return BlobRef{}, fmt.Errorf("put blob: close temp file: %w", err)
	}

	hash := hex.EncodeToString(hasher.Sum(nil))
	ref := BlobRef{Hash: hash, Size: size, MIME: declaredMIME}
	finalPath, err := s.path(ref)
	if err != nil {
		return BlobRef{}, fmt.Errorf("put blob: derive final path: %w", err)
	}
	shardDir := filepath.Dir(finalPath)
	if err := ensurePrivateDir(shardDir); err != nil {
		return BlobRef{}, fmt.Errorf("put blob: create hash shard: %w", err)
	}
	// Make creation of the shard durable before publishing an entry within it.
	if err := syncDir(s.root); err != nil {
		return BlobRef{}, fmt.Errorf("put blob: sync root directory: %w", err)
	}

	stageDir, err = os.MkdirTemp(shardDir, ".blob-stage-*")
	if err != nil {
		return BlobRef{}, fmt.Errorf("put blob: create staging directory: %w", err)
	}
	if err := os.Chmod(stageDir, privateDirMode); err != nil {
		return BlobRef{}, fmt.Errorf("put blob: set staging directory mode: %w", err)
	}
	stagePath := filepath.Join(stageDir, "blob")
	if err := os.Rename(tmpPath, stagePath); err != nil {
		return BlobRef{}, fmt.Errorf("put blob: stage temp file: %w", err)
	}
	tmpPath = stagePath

	// Link publishes without replacing an existing inode. Concurrent puts of
	// identical bytes therefore converge on one final file, including across
	// different BlobStore instances using the same root.
	for {
		linkErr := os.Link(stagePath, finalPath)
		switch {
		case linkErr == nil:
		case errors.Is(linkErr, fs.ErrExist):
			if err := validateExistingBlob(finalPath, size); err != nil {
				// A Delete through another BlobStore may remove the entry
				// between Link's EEXIST result and validation. Retry publication
				// only for that precise race; preserve all other stat errors.
				if errors.Is(err, fs.ErrNotExist) {
					if ctxErr := ctx.Err(); ctxErr != nil {
						return BlobRef{}, fmt.Errorf("put blob: %w", ctxErr)
					}
					continue
				}
				return BlobRef{}, fmt.Errorf("put blob: validate deduplicated blob: %w", err)
			}
		default:
			return BlobRef{}, fmt.Errorf("put blob: publish blob: %w", linkErr)
		}
		break
	}

	if err := os.Remove(stagePath); err != nil {
		return BlobRef{}, fmt.Errorf("put blob: remove staged file: %w", err)
	}
	tmpPath = ""
	if err := os.Remove(stageDir); err != nil {
		return BlobRef{}, fmt.Errorf("put blob: remove staging directory: %w", err)
	}
	stageDir = ""
	if err := syncDir(shardDir); err != nil {
		return BlobRef{}, fmt.Errorf("put blob: sync hash shard: %w", err)
	}
	// The staging rename removed the original temp entry from root. Sync both
	// sides of that cross-directory rename before reporting a durable Put.
	if err := syncDir(s.root); err != nil {
		return BlobRef{}, fmt.Errorf("put blob: sync root directory after publish: %w", err)
	}

	return ref, nil
}

// Open opens the regular file identified by ref for reading.
func (s *BlobStore) Open(ref BlobRef) (io.ReadCloser, error) {
	if s == nil {
		return nil, fmt.Errorf("open blob: store is nil")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	path, err := s.path(ref)
	if err != nil {
		return nil, fmt.Errorf("open blob: %w", err)
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("open blob: %w", err)
	}
	if !pathInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("open blob: hash path is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open blob: %w", err)
	}
	fileInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("open blob: stat file: %w", err)
	}
	if !fileInfo.Mode().IsRegular() || !os.SameFile(pathInfo, fileInfo) {
		_ = file.Close()
		return nil, fmt.Errorf("open blob: hash path changed while opening")
	}
	return file, nil
}

// Stat returns file information for the regular file identified by ref.
func (s *BlobStore) Stat(ref BlobRef) (fs.FileInfo, error) {
	if s == nil {
		return nil, fmt.Errorf("stat blob: store is nil")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	path, err := s.path(ref)
	if err != nil {
		return nil, fmt.Errorf("stat blob: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat blob: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("stat blob: hash path is not a regular file")
	}
	return info, nil
}

// Delete removes the blob identified by ref.
func (s *BlobStore) Delete(ref BlobRef) error {
	if s == nil {
		return fmt.Errorf("delete blob: store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path(ref)
	if err != nil {
		return fmt.Errorf("delete blob: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("delete blob: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("delete blob: hash path is not a regular file")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("delete blob: %w", err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("delete blob: sync hash shard: %w", err)
	}
	return nil
}

func (s *BlobStore) path(ref BlobRef) (string, error) {
	if s == nil {
		return "", fmt.Errorf("store is nil")
	}
	if err := validateHash(ref.Hash); err != nil {
		return "", err
	}
	return filepath.Join(s.root, ref.Hash[:2], ref.Hash[2:]), nil
}

func validateHash(hash string) error {
	if len(hash) != sha256.Size*2 {
		return fmt.Errorf("invalid blob hash: expected %d lowercase hex characters", sha256.Size*2)
	}
	for _, char := range hash {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return fmt.Errorf("invalid blob hash: expected lowercase SHA-256 hex")
		}
	}
	return nil
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, privateDirMode); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("path is not a private directory")
	}
	if err := os.Chmod(path, privateDirMode); err != nil {
		return fmt.Errorf("set directory mode: %w", err)
	}
	return nil
}

func validateExistingBlob(path string, expectedSize int64) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("hash path is not a regular file")
	}
	if info.Mode().Perm() != privateFileMode {
		return fmt.Errorf("hash path mode is %04o, want %04o", info.Mode().Perm(), privateFileMode)
	}
	if info.Size() != expectedSize {
		return fmt.Errorf("hash path size is %d, want %d", info.Size(), expectedSize)
	}
	return nil
}

func copyCapped(ctx context.Context, dst io.Writer, src io.Reader, max int64) (int64, error) {
	buffer := make([]byte, copyBufferSize)
	var written int64
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}

		remaining := max - written
		readSize := len(buffer)
		if remaining < int64(readSize) {
			// remaining is non-negative and smaller than the buffer, so this
			// conversion cannot overflow. The extra byte detects max+1.
			readSize = int(remaining) + 1
		}
		n, readErr := src.Read(buffer[:readSize])
		if n < 0 || n > readSize {
			return written, fmt.Errorf("reader returned invalid byte count %d", n)
		}
		if n > 0 {
			emptyReads = 0
			if int64(n) > remaining {
				return written, &ErrTooLarge{Max: max}
			}
			writeN, writeErr := dst.Write(buffer[:n])
			if writeN < 0 || writeN > n {
				return written, fmt.Errorf("writer returned invalid byte count %d", writeN)
			}
			written += int64(writeN)
			if writeErr != nil {
				return written, writeErr
			}
			if writeN != n {
				return written, io.ErrShortWrite
			}
		} else {
			emptyReads++
			if emptyReads >= 100 {
				return written, io.ErrNoProgress
			}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, readErr
		}
	}
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
