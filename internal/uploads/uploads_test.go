package uploads

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveDefaultsToWorkspaceAndAvoidsOverwrite(t *testing.T) {
	root := t.TempDir()
	path, err := Save(root, "report.txt", []byte("first"))
	if err != nil || path != "report.txt" {
		t.Fatalf("Save = %q, %v", path, err)
	}
	path, err = Save(root, "report.txt", []byte("second"))
	if err != nil || path != "report (1).txt" {
		t.Fatalf("second Save = %q, %v", path, err)
	}
	data, err := os.ReadFile(filepath.Join(root, path))
	if err != nil || string(data) != "second" {
		t.Fatalf("saved data = %q, %v", data, err)
	}
}

func TestSaveUsesConfiguredDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".tandem"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, SettingsPath), []byte(`{"uploads":{"directory":".tandem/uploads"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := Save(root, "photo.png", []byte("data"))
	if err != nil || path != ".tandem/uploads/photo.png" {
		t.Fatalf("Save = %q, %v", path, err)
	}
}

func TestSaveRejectsEscapingDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".tandem"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, SettingsPath), []byte(`{"uploads":{"directory":"../outside"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Save(root, "file", []byte("data")); err == nil {
		t.Fatal("Save succeeded with escaping directory")
	}
}
