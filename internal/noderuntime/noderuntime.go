// Package noderuntime downloads and manages a pinned Node.js distribution under
// TANDEM_HOME when the operator opts into a Tandem-managed Node runtime (as
// opposed to using a Node already installed on the host).
package noderuntime

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/aiguy110/tandem/internal/config"
)

// distBaseURL is the Node.js distribution index, overridable in tests.
const distBaseURL = "https://nodejs.org/dist"

// versionFile is the fingerprint file written into root once a managed Node
// install completes successfully.
const versionFile = ".tandem-node-version"

// Ensure makes a managed Node distribution of the given version available under
// root, whose executable layout matches config.ManagedNodePaths(root). It
// downloads and extracts the official build when absent and is idempotent and
// safe to call before every use.
func Ensure(ctx context.Context, root, version string, log io.Writer) error {
	if log == nil {
		log = io.Discard
	}

	nodePath, _ := config.ManagedNodePaths(root)
	if fingerprint, err := os.ReadFile(filepath.Join(root, versionFile)); err == nil {
		if strings.TrimSpace(string(fingerprint)) == version {
			if _, statErr := os.Stat(nodePath); statErr == nil {
				return nil
			}
		}
	}

	asset, err := assetName(version, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}

	fmt.Fprintf(log, "tandem: downloading Node v%s (%s)\n", version, asset)

	checksums, err := fetchChecksums(ctx, version)
	if err != nil {
		return fmt.Errorf("fetching Node checksums: %w", err)
	}
	expectedSum, ok := checksums[asset]
	if !ok {
		return fmt.Errorf("no checksum found for asset %q", asset)
	}

	archivePath, cleanup, err := downloadArchive(ctx, version, asset, expectedSum, log)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return fmt.Errorf("downloading Node archive: %w", err)
	}

	downloadDir := root + ".download"
	if err := os.RemoveAll(downloadDir); err != nil {
		return fmt.Errorf("clearing stale download dir: %w", err)
	}
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		return fmt.Errorf("creating download dir: %w", err)
	}

	stripPrefix := strings.TrimSuffix(strings.TrimSuffix(asset, ".tar.gz"), ".zip")

	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("opening downloaded archive: %w", err)
	}
	defer f.Close()

	if strings.HasSuffix(asset, ".zip") {
		fi, statErr := f.Stat()
		if statErr != nil {
			return fmt.Errorf("stat downloaded archive: %w", statErr)
		}
		if err := extractZip(f, fi.Size(), stripPrefix, downloadDir); err != nil {
			return fmt.Errorf("extracting Node archive: %w", err)
		}
	} else {
		if err := extractTarGz(f, stripPrefix, downloadDir); err != nil {
			return fmt.Errorf("extracting Node archive: %w", err)
		}
	}

	fmt.Fprintf(log, "tandem: installing Node v%s to %s\n", version, root)

	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("removing previous managed Node install: %w", err)
	}
	if err := os.Rename(downloadDir, root); err != nil {
		return fmt.Errorf("installing managed Node: %w", err)
	}

	if err := os.WriteFile(filepath.Join(root, versionFile), []byte(version), 0o644); err != nil {
		return fmt.Errorf("writing Node version fingerprint: %w", err)
	}

	fmt.Fprintf(log, "tandem: Node v%s ready\n", version)
	return nil
}

// nodeOSToken maps a Go GOOS to the token nodejs.org uses in its release
// filenames.
func nodeOSToken(goos string) string {
	if goos == "windows" {
		return "win"
	}
	return goos
}

// nodeArchToken maps a Go GOARCH to the token nodejs.org uses in its release
// filenames. It returns an error for architectures nodejs.org does not (or we
// do not) support.
func nodeArchToken(goarch string) (string, error) {
	switch goarch {
	case "amd64":
		return "x64", nil
	case "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported architecture %q for managed Node runtime", goarch)
	}
}

// assetName returns the nodejs.org release asset filename for the given
// version/os/arch, e.g. "node-v22.11.0-linux-x64.tar.gz".
func assetName(version, goos, goarch string) (string, error) {
	archToken, err := nodeArchToken(goarch)
	if err != nil {
		return "", err
	}
	osToken := nodeOSToken(goos)
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("node-v%s-%s-%s.%s", version, osToken, archToken, ext), nil
}

// parseChecksums parses the contents of a nodejs.org SHASUMS256.txt file into
// a map from filename to lowercase hex sha256 digest.
func parseChecksums(data []byte) map[string]string {
	sums := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		sum, name := fields[0], fields[1]
		sums[name] = strings.ToLower(sum)
	}
	return sums
}

// fetchChecksums downloads and parses SHASUMS256.txt for the given version.
func fetchChecksums(ctx context.Context, version string) (map[string]string, error) {
	url := fmt.Sprintf("%s/v%s/SHASUMS256.txt", distBaseURL, version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building checksum request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("requesting checksum file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s fetching checksum file", resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading checksum file: %w", err)
	}
	return parseChecksums(data), nil
}

// downloadArchive streams the Node archive for version/asset to a temp file,
// verifying its sha256 against expectedSum. It returns the path to the
// downloaded file and a cleanup func that removes it; callers should defer
// cleanup() regardless of error, since the file is only needed transiently.
func downloadArchive(ctx context.Context, version, asset, expectedSum string, log io.Writer) (path string, cleanup func(), err error) {
	url := fmt.Sprintf("%s/v%s/%s", distBaseURL, version, asset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", nil, fmt.Errorf("building download request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("requesting archive: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("unexpected status %s downloading %s", resp.Status, url)
	}

	tmp, err := os.CreateTemp("", "tandem-node-*.download")
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

	actualSum := hex.EncodeToString(h.Sum(nil))
	if actualSum != expectedSum {
		return "", cleanup, fmt.Errorf("checksum mismatch for %s: expected %s, got %s", asset, expectedSum, actualSum)
	}

	return tmp.Name(), cleanup, nil
}

// extractTarGz extracts a gzip-compressed tar archive from r into dest,
// stripping the leading stripPrefix path component from every entry (and
// skipping entries that don't have it, e.g. a top-level "LICENSE" placed
// alongside the versioned dir would be dropped).
func extractTarGz(r io.Reader, stripPrefix, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("opening gzip stream: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}

		rel, ok := stripTopLevel(hdr.Name, stripPrefix)
		if !ok || rel == "" {
			continue
		}
		target := filepath.Join(dest, rel)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)|0o700); err != nil {
				return fmt.Errorf("creating dir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("creating parent dir for %s: %w", target, err)
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return fmt.Errorf("creating file %s: %w", target, err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return fmt.Errorf("writing file %s: %w", target, err)
			}
			if err := out.Close(); err != nil {
				return fmt.Errorf("closing file %s: %w", target, err)
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("creating parent dir for symlink %s: %w", target, err)
			}
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("creating symlink %s: %w", target, err)
			}
		default:
			// Ignore other entry types (hard links, devices, etc.) that Node's
			// tarball does not use.
		}
	}
}

// extractZip extracts a zip archive (read from r, of the given size) into
// dest, stripping the leading stripPrefix path component from every entry.
func extractZip(r io.ReaderAt, size int64, stripPrefix, dest string) error {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return fmt.Errorf("opening zip archive: %w", err)
	}

	for _, f := range zr.File {
		rel, ok := stripTopLevel(f.Name, stripPrefix)
		if !ok || rel == "" {
			continue
		}
		target := filepath.Join(dest, rel)

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, f.Mode()|0o700); err != nil {
				return fmt.Errorf("creating dir %s: %w", target, err)
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("creating parent dir for %s: %w", target, err)
		}

		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("opening zip entry %s: %w", f.Name, err)
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			rc.Close()
			return fmt.Errorf("creating file %s: %w", target, err)
		}
		if _, err := io.Copy(out, rc); err != nil {
			out.Close()
			rc.Close()
			return fmt.Errorf("writing file %s: %w", target, err)
		}
		if err := out.Close(); err != nil {
			rc.Close()
			return fmt.Errorf("closing file %s: %w", target, err)
		}
		rc.Close()
	}
	return nil
}

// stripTopLevel removes the leading stripPrefix path component from name,
// e.g. stripTopLevel("node-v22.11.0-linux-x64/bin/node", "node-v22.11.0-linux-x64")
// returns ("bin/node", true). It returns ok=false if name is not under
// stripPrefix.
func stripTopLevel(name, stripPrefix string) (string, bool) {
	name = filepath.ToSlash(name)
	prefix := filepath.ToSlash(stripPrefix)
	if name == prefix {
		return "", true
	}
	prefixWithSlash := prefix + "/"
	if !strings.HasPrefix(name, prefixWithSlash) {
		return "", false
	}
	return strings.TrimPrefix(name, prefixWithSlash), true
}
