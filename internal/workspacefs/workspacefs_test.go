package workspacefs

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func openTestFS(t *testing.T) (*FS, string) {
	t.Helper()
	root := t.TempDir()
	f, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, root
}

func TestRoundTripAbsoluteAndLineRange(t *testing.T) {
	f, root := openTestFS(t)
	if err := f.WriteTextFile(filepath.Join("nested", "new", "note.txt"), "one\ntwo\nthree"); err != nil {
		t.Fatal(err)
	}
	line, limit := 2, 1
	got, err := f.ReadTextFile(filepath.Join(root, "nested", "new", "note.txt"), &line, &limit)
	if err != nil || got != "two" {
		t.Fatalf("range=%q err=%v", got, err)
	}
	all, err := f.ReadTextFile("nested/new/note.txt", nil, nil)
	if err != nil || all != "one\ntwo\nthree" {
		t.Fatalf("round trip=%q err=%v", all, err)
	}
}

func TestTraversalMapsToACPInvalidParams(t *testing.T) {
	f, root := openTestFS(t)
	for _, requested := range []string{"../secret", filepath.Join(root, "..", "secret")} {
		err := f.WriteTextFile(requested, "no")
		var rpc interface{ JSONRPCCode() int }
		if !IsPathEscape(err) || !errors.As(err, &rpc) || rpc.JSONRPCCode() != InvalidParamsCode {
			t.Errorf("%q error=%T %v", requested, err, err)
		}
	}
}

func TestSymlinkEscapesAndNonexistentDescendantsAreRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation generally requires elevated Windows privileges")
	}
	f, root := openTestFS(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadTextFile("escape/secret", nil, nil); !IsPathEscape(err) {
		t.Fatalf("read escape error=%T %v", err, err)
	}
	if err := f.WriteTextFile("escape/not/yet/created.txt", "no"); !IsPathEscape(err) {
		t.Fatalf("write escape error=%T %v", err, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "not")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside tree was touched: %v", err)
	}
}

func TestParentSwapCannotRedirectWriteOutside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation generally requires elevated Windows privileges")
	}
	f, root := openTestFS(t)
	outside := t.TempDir()
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate a parent changing after validation but before use. Descriptor-
	// relative Root operations must reject the replacement escape symlink.
	if err := os.Rename(parent, filepath.Join(root, "moved-parent")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteTextFile("parent/raced.txt", "no"); !IsPathEscape(err) {
		t.Fatalf("parent-swap error=%T %v", err, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "raced.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("race escaped workspace: %v", err)
	}
}

func TestContainedRelativeSymlinkWorks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation generally requires elevated Windows privileges")
	}
	f, root := openTestFS(t)
	if err := os.Mkdir(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteTextFile("link/inside.txt", "ok"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "real", "inside.txt")); err != nil || string(data) != "ok" {
		t.Fatalf("contained symlink=%q err=%v", data, err)
	}
}

func TestReadDirStaysContained(t *testing.T) {
	f, root := openTestFS(t)
	if err := os.WriteFile(filepath.Join(root, "visible.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := f.ReadDir(".")
	if err != nil || len(entries) != 1 || entries[0].Name() != "visible.txt" {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	if _, err := f.ReadDir("../outside"); !IsPathEscape(err) {
		t.Fatalf("escape error=%T %v", err, err)
	}
}
