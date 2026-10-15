package bunruntime

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aiguy110/tandem/internal/config"
)

func TestAssetName(t *testing.T) {
	cases := []struct {
		goos, goarch string
		musl, avx2   bool
		want         string
		wantErr      bool
	}{
		{"linux", "amd64", false, true, "bun-linux-x64.zip", false},
		{"linux", "amd64", false, false, "bun-linux-x64-baseline.zip", false},
		{"linux", "amd64", true, true, "bun-linux-x64-musl.zip", false},
		{"linux", "amd64", true, false, "bun-linux-x64-musl-baseline.zip", false},
		{"linux", "arm64", false, true, "bun-linux-aarch64.zip", false},
		{"linux", "arm64", true, true, "bun-linux-aarch64-musl.zip", false},
		{"darwin", "arm64", false, true, "bun-darwin-aarch64.zip", false},
		{"darwin", "amd64", false, true, "bun-darwin-x64.zip", false},
		{"windows", "amd64", false, true, "bun-windows-x64.zip", false},
		{"linux", "386", false, true, "", true},
		{"plan9", "amd64", false, true, "", true},
	}
	for _, c := range cases {
		got, err := assetName(c.goos, c.goarch, c.musl, c.avx2)
		if c.wantErr != (err != nil) || got != c.want {
			t.Errorf("assetName(%s,%s,musl=%v,avx2=%v) = %q, %v; want %q (err=%v)", c.goos, c.goarch, c.musl, c.avx2, got, err, c.want, c.wantErr)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	got := parseChecksums([]byte("ABC  bun-linux-x64.zip\n\nmalformed\ndef  bun-darwin-aarch64.zip\n"))
	if len(got) != 2 || got["bun-linux-x64.zip"] != "abc" || got["bun-darwin-aarch64.zip"] != "def" {
		t.Fatalf("parseChecksums = %#v", got)
	}
}

// TestEnsureDownloadsVerifiesAndIsIdempotent serves a fake release and checks
// that Ensure installs the binary, refuses a bad checksum, and skips the
// network once the pinned version is present.
func TestEnsureDownloadsVerifiesAndIsIdempotent(t *testing.T) {
	asset, err := assetName(runtime.GOOS, runtime.GOARCH, isMusl(), hasAVX2())
	if err != nil {
		t.Skip(err)
	}
	binary := filepath.Base(config.ManagedBunPath(t.TempDir()))
	archive := t.TempDir() + "/bun.zip"
	f, _ := os.Create(archive)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("bun-platform/" + binary)
	w.Write([]byte("#!/bin/sh\necho fake bun\n"))
	zw.Close()
	f.Close()
	body, _ := os.ReadFile(archive)
	sum := sha256.Sum256(body)
	goodSum := hex.EncodeToString(sum[:])

	requests := 0
	checksum := goodSum
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/bun-v1.2.3/SHASUMS256.txt":
			fmt.Fprintf(w, "%s  %s\n", checksum, asset)
		case "/bun-v1.2.3/" + asset:
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := releaseBaseURL
	releaseBaseURL = srv.URL
	defer func() { releaseBaseURL = old }()

	root := filepath.Join(t.TempDir(), "bun")
	checksum = "0000"
	if err := Ensure(context.Background(), root, "1.2.3", nil); err == nil {
		t.Fatal("Ensure accepted an archive with a mismatched checksum")
	}
	checksum = goodSum
	if err := Ensure(context.Background(), root, "1.2.3", nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(config.ManagedBunPath(root))
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("managed bun not installed executable: %v %v", info, err)
	}
	before := requests
	if err := Ensure(context.Background(), root, "1.2.3", nil); err != nil {
		t.Fatal(err)
	}
	if requests != before {
		t.Fatal("Ensure hit the network for an already-installed version")
	}
}
