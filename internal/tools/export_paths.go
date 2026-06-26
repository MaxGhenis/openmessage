package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	exportDirEnv       = "OPENMESSAGES_EXPORT_DIR"
	allowAnyExportPath = "OPENMESSAGES_ALLOW_ANY_EXPORT_PATH"
)

func resolveExportWritePath(requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", fmt.Errorf("output_path is required")
	}
	if allowAnyExportPaths() {
		return filepath.Abs(requested)
	}
	return resolvePathInsideExportRoot(requested, true)
}

func resolveExportReadPath(requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", fmt.Errorf("path is required")
	}
	if allowAnyExportPaths() {
		return filepath.Abs(requested)
	}
	return resolvePathInsideExportRoot(requested, false)
}

func writePrivateExportFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".openmessage-export-*")
	if err != nil {
		return fmt.Errorf("create temp output: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp output: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp output: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp output: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace output: %w", err)
	}
	return nil
}

func allowAnyExportPaths() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(allowAnyExportPath))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func resolvePathInsideExportRoot(requested string, forWrite bool) (string, error) {
	root, err := exportRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create export root: %w", err)
	}
	candidate := requested
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	if err := ensureInside(root, candidate); err != nil {
		return "", err
	}
	if forWrite {
		if err := ensureWritableParentInside(root, candidate); err != nil {
			return "", err
		}
	} else {
		if err := ensureRealPathInside(root, candidate, false); err != nil {
			return "", err
		}
	}
	return candidate, nil
}

func exportRoot() (string, error) {
	root := strings.TrimSpace(os.Getenv(exportDirEnv))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		root = filepath.Join(home, "Documents", "OpenMessage")
	}
	return filepath.Abs(root)
}

func ensureRealPathInside(root, candidate string, forWrite bool) error {
	rootEval, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve export root: %w", err)
	}
	parent := candidate
	if forWrite {
		parent = filepath.Dir(candidate)
	}
	parentEval, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	if err := ensureInside(rootEval, parentEval); err != nil {
		return err
	}
	if st, err := os.Lstat(candidate); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("path must not be a symlink: %s", candidate)
	}
	return nil
}

func ensureWritableParentInside(root, candidate string) error {
	rootEval, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve export root: %w", err)
	}
	parent := filepath.Dir(candidate)
	existing := parent
	for {
		if st, err := os.Lstat(existing); err == nil {
			if st.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("path must not traverse a symlink: %s", existing)
			}
			if !st.IsDir() {
				return fmt.Errorf("path parent is not a directory: %s", existing)
			}
			break
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect path: %w", err)
		}
		next := filepath.Dir(existing)
		if next == existing {
			return fmt.Errorf("resolve path parent: %s", parent)
		}
		existing = next
	}
	existingEval, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	if err := ensureInside(rootEval, existingEval); err != nil {
		return err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	if st, err := os.Lstat(candidate); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("path must not be a symlink: %s", candidate)
	}
	return nil
}

func ensureInside(root, candidate string) error {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %s is outside export root %s; set %s=1 to override", candidate, root, allowAnyExportPath)
	}
	return nil
}
