// Package bunruntime downloads and manages a pinned Bun distribution under
// TANDEM_HOME. Bun is Tandem's JavaScript runtime and package manager: it
// launches the ACP bridges, Playwright MCP, history importers and automation
// scripts, and installs their packages. The managed copy is the default so a
// host needs no preinstalled JavaScript toolchain; operators may opt into a
// Bun already on the host instead.
package bunruntime

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/sys/cpu"

	"github.com/aiguy110/tandem/internal/config"
)

// releaseBaseURL is Bun's GitHub release download root, overridable in tests.
var releaseBaseURL = "https://github.com/oven-sh/bun/releases/download"

// versionFile is the fingerprint file written into root once a managed Bun
// install completes successfully.
const versionFile = ".tandem-bun-version"

// ensureMu serializes Ensure so concurrent first spawns do not race on the
// download directory.
var ensureMu sync.Mutex

// Ensure makes a managed Bun of the given version available under root, at
// config.ManagedBunPath(root). It downloads and verifies the official build
// when absent, and is idempotent and cheap enough to call before every use.
func Ensure(ctx context.Context, root, version string, log io.Writer) error {
	if log == nil {
		log = io.Discard
	}
	bunPath := config.ManagedBunPath(root)
	if installed(root, version, bunPath) {
		return nil
	}
	ensureMu.Lock()
	defer ensureMu.Unlock()
	if installed(root, version, bunPath) {
		return nil
	}

	asset, err := assetName(runtime.GOOS, runtime.GOARCH, isMusl(), hasAVX2())
	if err != nil {
		return err
	}
	slog.Info("downloading managed bun runtime", "version", version, "asset", asset, "root", root)
	fmt.Fprintf(log, "tandem: downloading Bun v%s (%s)\n", version, asset)

	checksums, err := fetchChecksums(ctx, version)
	if err != nil {
		return fmt.Errorf("fetching Bun checksums: %w", err)
	}
	expectedSum, ok := checksums[asset]
	if !ok {
		return fmt.Errorf("no checksum found for Bun asset %q", asset)
	}
	archivePath, cleanup, err := downloadArchive(ctx, version, asset, expectedSum)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return fmt.Errorf("downloading Bun archive: %w", err)
	}

	downloadDir := root + ".download"
	if err := os.RemoveAll(downloadDir); err != nil {
		return fmt.Errorf("clearing stale download dir: %w", err)
	}
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		return fmt.Errorf("creating download dir: %w", err)
	}
	if err := extractBinary(archivePath, filepath.Base(bunPath), filepath.Join(downloadDir, filepath.Base(bunPath))); err != nil {
		os.RemoveAll(downloadDir)
		return fmt.Errorf("extracting Bun archive: %w", err)
	}
	if err := os.WriteFile(filepath.Join(downloadDir, versionFile), []byte(version), 0o644); err != nil {
		os.RemoveAll(downloadDir)
		return fmt.Errorf("writing Bun version fingerprint: %w", err)
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("removing previous managed Bun install: %w", err)
	}
	if err := os.Rename(downloadDir, root); err != nil {
		return fmt.Errorf("installing managed Bun: %w", err)
	}
	slog.Info("managed bun runtime ready", "version", version, "path", bunPath)
	fmt.Fprintf(log, "tandem: Bun v%s ready\n", version)
	return nil
}

func installed(root, version, bunPath string) bool {
	fingerprint, err := os.ReadFile(filepath.Join(root, versionFile))
	if err != nil || strings.TrimSpace(string(fingerprint)) != version {
		return false
	}
	info, err := os.Stat(bunPath)
	return err == nil && !info.IsDir()
}

// assetName returns Bun's release asset for a platform, e.g.
// "bun-linux-x64-musl-baseline.zip". Baseline builds avoid AVX2, which the
// default x64 builds require.
func assetName(goos, goarch string, musl, avx2 bool) (string, error) {
	var arch string
	switch goarch {
	case "amd64":
		arch = "x64"
	case "arm64":
		arch = "aarch64"
	default:
		return "", fmt.Errorf("unsupported architecture %q for managed Bun runtime", goarch)
	}
	switch goos {
	case "linux", "darwin", "windows":
	default:
		return "", fmt.Errorf("unsupported OS %q for managed Bun runtime", goos)
	}
	name := "bun-" + goos + "-" + arch
	if goos == "linux" && musl {
		name += "-musl"
	}
	if arch == "x64" && !avx2 {
		name += "-baseline"
	}
	return name + ".zip", nil
}

// isMusl reports whether this Linux host uses musl libc (e.g. Alpine), which
// needs Bun's musl build. glibc distributions can carry musl's loader too (the
// musl package installs it), so musl is only chosen when no glibc loader exists.
func isMusl() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	return hasLoader("/lib/ld-musl-*.so.1") && !hasLoader(
		"/lib64/ld-linux-*.so.*", "/lib/ld-linux-*.so.*", "/lib/*-linux-gnu*/ld-linux-*.so.*")
}

func hasLoader(patterns ...string) bool {
	for _, pattern := range patterns {
		if matches, _ := filepath.Glob(pattern); len(matches) > 0 {
			return true
		}
	}
	return false
}

func hasAVX2() bool { return runtime.GOARCH != "amd64" || cpu.X86.HasAVX2 }

// parseChecksums parses a SHASUMS256.txt file into filename -> lowercase hex.
func parseChecksums(data []byte) map[string]string {
	sums := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		sums[fields[1]] = strings.ToLower(fields[0])
	}
	return sums
}

func releaseURL(version, file string) string {
	return fmt.Sprintf("%s/bun-v%s/%s", releaseBaseURL, version, file)
}

func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("unexpected status %s fetching %s", resp.Status, url)
	}
	return resp, nil
}

func fetchChecksums(ctx context.Context, version string) (map[string]string, error) {
	resp, err := get(ctx, releaseURL(version, "SHASUMS256.txt"))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading checksum file: %w", err)
	}
	return parseChecksums(data), nil
}

// downloadArchive streams the asset to a temp file and verifies its sha256.
// Callers should defer cleanup() regardless of error.
func downloadArchive(ctx context.Context, version, asset, expectedSum string) (path string, cleanup func(), err error) {
	resp, err := get(ctx, releaseURL(version, asset))
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	tmp, err := os.CreateTemp("", "tandem-bun-*.download")
	if err != nil {
		return "", nil, fmt.Errorf("creating temp file: %w", err)
	}
	cleanup = func() { os.Remove(tmp.Name()) }
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return "", cleanup, fmt.Errorf("writing archive to disk: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", cleanup, fmt.Errorf("closing downloaded archive: %w", err)
	}
	if actual := hex.EncodeToString(h.Sum(nil)); actual != expectedSum {
		return "", cleanup, fmt.Errorf("checksum mismatch for %s: expected %s, got %s", asset, expectedSum, actual)
	}
	return tmp.Name(), cleanup, nil
}

// extractBinary copies the single entry named binary (at any depth; Bun's
// archives hold "bun-<platform>/bun") out of the zip at archivePath to dest.
func extractBinary(archivePath, binary, dest string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("opening zip archive: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || filepath.Base(filepath.FromSlash(f.Name)) != binary {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("opening zip entry %s: %w", f.Name, err)
		}
		defer rc.Close()
		out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			return fmt.Errorf("creating %s: %w", dest, err)
		}
		if _, err := io.Copy(out, rc); err != nil {
			out.Close()
			return fmt.Errorf("writing %s: %w", dest, err)
		}
		return out.Close()
	}
	return fmt.Errorf("archive has no %s executable", binary)
}
