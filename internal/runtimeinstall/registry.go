package runtimeinstall

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// defaultRegistry is the public npm registry. Bun installs packages from it
// too, so version lookups and installs agree on what exists.
const defaultRegistry = "https://registry.npmjs.org/"

// registryURL is overridable in tests.
var registryURL = func() string {
	for _, key := range []string{"NPM_CONFIG_REGISTRY", "npm_config_registry", "BUN_CONFIG_REGISTRY"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return defaultRegistry
}

var registryClient = &http.Client{Timeout: 30 * time.Second}

// packument is the subset of an npm registry package document Tandem reads.
type packument struct {
	Versions map[string]struct {
		Dist struct {
			Integrity string `json:"integrity"`
		} `json:"dist"`
	} `json:"versions"`
}

// fetchPackument reads a package's abbreviated metadata straight from the
// registry. Querying over HTTP rather than through a package-manager CLI keeps
// update checks independent of the JS runtime being installed yet.
func fetchPackument(ctx context.Context, pkg string) (packument, error) {
	base := strings.TrimSuffix(registryURL(), "/")
	// Scoped names escape their "/" (@scope%2Fname).
	endpoint := base + "/" + url.PathEscape(pkg)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return packument{}, fmt.Errorf("inspect %s: %w", pkg, err)
	}
	req.Header.Set("Accept", "application/vnd.npm.install-v1+json; q=1.0, application/json; q=0.8")
	started := time.Now()
	resp, err := registryClient.Do(req)
	if err != nil {
		slog.Warn("npm registry request failed", "package", pkg, "error", err)
		return packument{}, fmt.Errorf("inspect %s: %w", pkg, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("npm registry returned an error", "package", pkg, "status", resp.Status)
		return packument{}, fmt.Errorf("inspect %s: registry returned %s", pkg, resp.Status)
	}
	var doc packument
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&doc); err != nil {
		return packument{}, fmt.Errorf("inspect %s: %w", pkg, err)
	}
	slog.Debug("fetched npm package metadata", "package", pkg, "versions", len(doc.Versions), "elapsed", time.Since(started))
	return doc, nil
}

// publishedVersions lists every version of a package, newest first. Tandem
// decides for itself what is newest and what is compatible; see semver.go for
// why delegating range resolution to a package manager is unsound.
func publishedVersions(ctx context.Context, pkg string) ([]string, error) {
	doc, err := fetchPackument(ctx, pkg)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(doc.Versions))
	for v := range doc.Versions {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("inspect %s: registry returned no versions", pkg)
	}
	sortVersions(out)
	return out, nil
}

// publishedIntegrity returns the registry's integrity hash for one version, or
// "" when it cannot be read. It is recorded for provenance, not enforced here:
// the package manager verifies tarballs itself.
func publishedIntegrity(ctx context.Context, pkg, version string) string {
	doc, err := fetchPackument(ctx, pkg)
	if err != nil {
		return ""
	}
	return doc.Versions[version].Dist.Integrity
}
