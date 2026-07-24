package noderuntime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssetName(t *testing.T) {
	cases := []struct {
		version, goos, goarch string
		want                  string
		wantErr               bool
	}{
		{"22.11.0", "linux", "amd64", "node-v22.11.0-linux-x64.tar.gz", false},
		{"22.11.0", "linux", "arm64", "node-v22.11.0-linux-arm64.tar.gz", false},
		{"22.11.0", "darwin", "amd64", "node-v22.11.0-darwin-x64.tar.gz", false},
		{"22.11.0", "darwin", "arm64", "node-v22.11.0-darwin-arm64.tar.gz", false},
		{"22.11.0", "windows", "amd64", "node-v22.11.0-win-x64.zip", false},
		{"22.11.0", "windows", "arm64", "node-v22.11.0-win-arm64.zip", false},
		{"22.11.0", "linux", "386", "", true},
		{"22.11.0", "linux", "riscv64", "", true},
	}

	for _, c := range cases {
		got, err := assetName(c.version, c.goos, c.goarch)
		if c.wantErr {
			if err == nil {
				t.Errorf("assetName(%q,%q,%q): expected error, got %q", c.version, c.goos, c.goarch, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("assetName(%q,%q,%q): unexpected error: %v", c.version, c.goos, c.goarch, err)
			continue
		}
		if got != c.want {
			t.Errorf("assetName(%q,%q,%q) = %q, want %q", c.version, c.goos, c.goarch, got, c.want)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	data := []byte(strings.Join([]string{
		"abc123def456  node-v22.11.0-linux-x64.tar.gz",
		"789fed321cba  node-v22.11.0-darwin-arm64.tar.gz",
		"",
		"   ",
		"malformedline",
		"deadbeef  node-v22.11.0-win-x64.zip",
	}, "\n"))

	got := parseChecksums(data)

	want := map[string]string{
		"node-v22.11.0-linux-x64.tar.gz":    "abc123def456",
		"node-v22.11.0-darwin-arm64.tar.gz": "789fed321cba",
		"node-v22.11.0-win-x64.zip":         "deadbeef",
	}

	if len(got) != len(want) {
		t.Fatalf("parseChecksums() returned %d entries, want %d: %#v", len(got), len(want), got)
	}
	for name, sum := range want {
		if got[name] != sum {
			t.Errorf("parseChecksums()[%q] = %q, want %q", name, got[name], sum)
		}
	}
}

func TestExtractTarGz(t *testing.T) {
	const stripPrefix = "node-v22.11.0-linux-x64"

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	writeEntry := func(hdr *tar.Header, content []byte) {
		hdr.Size = int64(len(content))
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar header for %s: %v", hdr.Name, err)
		}
		if len(content) > 0 {
			if _, err := tw.Write(content); err != nil {
				t.Fatalf("writing tar content for %s: %v", hdr.Name, err)
			}
		}
	}

	writeEntry(&tar.Header{
		Name:     stripPrefix + "/bin",
		Typeflag: tar.TypeDir,
		Mode:     0o755,
	}, nil)
	writeEntry(&tar.Header{
		Name:     stripPrefix + "/bin/node",
		Typeflag: tar.TypeReg,
		Mode:     0o755,
	}, []byte("fake node binary"))
	writeEntry(&tar.Header{
		Name:     stripPrefix + "/README.md",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
	}, []byte("readme contents"))
	writeEntry(&tar.Header{
		Name:     stripPrefix + "/bin/npm",
		Typeflag: tar.TypeSymlink,
		Linkname: "../lib/node_modules/npm/bin/npm-cli.js",
		Mode:     0o777,
	}, nil)

	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip writer: %v", err)
	}

	dest := t.TempDir()
	if err := extractTarGz(&buf, stripPrefix, dest); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}

	nodePath := filepath.Join(dest, "bin", "node")
	fi, err := os.Stat(nodePath)
	if err != nil {
		t.Fatalf("stat %s: %v", nodePath, err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("bin/node mode = %o, want %o", fi.Mode().Perm(), 0o755)
	}
	content, err := os.ReadFile(nodePath)
	if err != nil {
		t.Fatalf("read %s: %v", nodePath, err)
	}
	if string(content) != "fake node binary" {
		t.Errorf("bin/node content = %q, want %q", content, "fake node binary")
	}

	readmePath := filepath.Join(dest, "README.md")
	if _, err := os.Stat(readmePath); err != nil {
		t.Fatalf("stat %s: %v", readmePath, err)
	}

	binDir := filepath.Join(dest, "bin")
	dirInfo, err := os.Stat(binDir)
	if err != nil {
		t.Fatalf("stat %s: %v", binDir, err)
	}
	if !dirInfo.IsDir() {
		t.Errorf("%s is not a directory", binDir)
	}

	npmPath := filepath.Join(dest, "bin", "npm")
	linkInfo, err := os.Lstat(npmPath)
	if err != nil {
		t.Fatalf("lstat %s: %v", npmPath, err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink", npmPath)
	}
	target, err := os.Readlink(npmPath)
	if err != nil {
		t.Fatalf("readlink %s: %v", npmPath, err)
	}
	if target != "../lib/node_modules/npm/bin/npm-cli.js" {
		t.Errorf("symlink target = %q, want %q", target, "../lib/node_modules/npm/bin/npm-cli.js")
	}
}

func TestStripTopLevel(t *testing.T) {
	cases := []struct {
		name, prefix string
		wantRel      string
		wantOK       bool
	}{
		{"node-v22.11.0-linux-x64/bin/node", "node-v22.11.0-linux-x64", "bin/node", true},
		{"node-v22.11.0-linux-x64", "node-v22.11.0-linux-x64", "", true},
		{"other-dir/bin/node", "node-v22.11.0-linux-x64", "", false},
	}
	for _, c := range cases {
		rel, ok := stripTopLevel(c.name, c.prefix)
		if ok != c.wantOK || rel != c.wantRel {
			t.Errorf("stripTopLevel(%q, %q) = (%q, %v), want (%q, %v)", c.name, c.prefix, rel, ok, c.wantRel, c.wantOK)
		}
	}
}
