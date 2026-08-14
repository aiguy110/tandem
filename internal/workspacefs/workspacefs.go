// Package workspacefs is the single filesystem containment boundary for ACP
// filesystem services. It deliberately uses os.Root rather than a lexical
// prefix check: Root resolves every operation relative to an open directory
// handle and prevents symlinks and concurrent parent renames from escaping it.
package workspacefs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const InvalidParamsCode = -32602

// PathEscapeError identifies requests that must map to JSON-RPC invalid params
// at the ACP adapter boundary.
type PathEscapeError struct {
	Requested string
	Root      string
	Cause     error
}

func (e *PathEscapeError) Error() string {
	return fmt.Sprintf("path escapes workspace: %s is not inside %s", e.Requested, e.Root)
}

func (e *PathEscapeError) Unwrap() error { return e.Cause }

// JSONRPCCode lets the ACP adapter preserve Node's invalid-parameter mapping
// without depending on filesystem implementation details.
func (e *PathEscapeError) JSONRPCCode() int { return InvalidParamsCode }

// IsPathEscape reports whether err originated at the containment boundary.
func IsPathEscape(err error) bool {
	var target *PathEscapeError
	return errors.As(err, &target)
}

type FS struct {
	rootName string
	root     *os.Root
}

// Open creates a contained filesystem rooted at an existing directory. OpenRoot
// follows a symlink used as the workspace root once, then pins the resulting
// directory for the lifetime of FS.
func Open(root string) (*FS, error) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	r, err := os.OpenRoot(real)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	return &FS{rootName: filepath.Clean(real), root: r}, nil
}

func (f *FS) Close() error { return f.root.Close() }
func (f *FS) Root() string { return f.rootName }

func (f *FS) relative(requested string) (string, error) {
	if requested == "" {
		requested = "."
	}
	var rel string
	var err error
	if filepath.IsAbs(requested) {
		rel, err = filepath.Rel(f.rootName, filepath.Clean(requested))
		if err != nil {
			return "", &PathEscapeError{Requested: requested, Root: f.rootName, Cause: err}
		}
	} else {
		rel = filepath.Clean(requested)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", &PathEscapeError{Requested: requested, Root: f.rootName}
	}
	return rel, nil
}

func (f *FS) pathError(requested string, err error) error {
	if err == nil {
		return nil
	}
	// os.Root uses ErrInvalid for malformed traversal on some platforms and an
	// unexported "path escapes from parent" sentinel for symlink escapes on
	// descriptor-backed platforms. Ordinary missing and permission errors retain
	// their native identity.
	if errors.Is(err, fs.ErrInvalid) || strings.Contains(err.Error(), "path escapes from parent") {
		return &PathEscapeError{Requested: requested, Root: f.rootName, Cause: err}
	}
	return err
}

func (f *FS) ReadTextFile(requested string, line, limit *int) (string, error) {
	rel, err := f.relative(requested)
	if err != nil {
		return "", err
	}
	file, err := f.root.Open(rel)
	if err != nil {
		return "", f.pathError(requested, err)
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return "", readErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	text := string(data)
	if line == nil && limit == nil {
		return text, nil
	}
	lines := strings.Split(text, "\n")
	start := 0
	if line != nil && *line > 1 {
		start = *line - 1
	}
	if start > len(lines) {
		start = len(lines)
	}
	end := len(lines)
	if limit != nil {
		if *limit <= 0 {
			end = start
		} else if start+*limit < end {
			end = start + *limit
		}
	}
	return strings.Join(lines[start:end], "\n"), nil
}

func (f *FS) WriteTextFile(requested, content string) error {
	rel, err := f.relative(requested)
	if err != nil {
		return err
	}
	parent := filepath.Dir(rel)
	if parent != "." {
		current := ""
		for _, component := range strings.Split(parent, string(filepath.Separator)) {
			current = filepath.Join(current, component)
			if err := f.root.Mkdir(current, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return f.pathError(requested, err)
			}
		}
	}
	file, err := f.root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return f.pathError(requested, err)
	}
	_, writeErr := file.WriteString(content)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// ReadDir returns the immediate entries of requested, while preserving the
// same descriptor-relative containment guarantees as the ACP file services.
func (f *FS) ReadDir(requested string) ([]fs.DirEntry, error) {
	rel, err := f.relative(requested)
	if err != nil {
		return nil, err
	}
	dir, err := f.root.Open(rel)
	if err != nil {
		return nil, f.pathError(requested, err)
	}
	entries, readErr := dir.ReadDir(-1)
	closeErr := dir.Close()
	if readErr != nil {
		return nil, f.pathError(requested, readErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return entries, nil
}
