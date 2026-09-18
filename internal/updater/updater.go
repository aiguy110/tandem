// Package updater checks GitHub releases and can replace the Tandem binary.
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const defaultRepository = "aiguy110/tandem"

const updateSetupTimeout = 5 * time.Second

const selfUpdateMarkerSuffix = ".self-update"

var developmentVersion = regexp.MustCompile(`^v?\d+\.\d+\.\d+\.[0-9a-f]{8}$`)

// Options supplies the process-specific dependencies used by CheckAtStartup.
// Zero values select the production defaults.
type Options struct {
	CurrentVersion string
	Repository     string
	APIBaseURL     string
	GOOS           string
	GOARCH         string
	Log            io.Writer
	HTTPClient     *http.Client
	Executable     string
}

type release struct {
	TagName string  `json:"tag_name"`
	Assets  []asset `json:"assets"`
}

type asset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
}

// CheckResult describes the latest release without downloading any assets.
type CheckResult struct {
	CurrentVersion string
	LatestVersion  string
	Available      bool
}

// Check queries the latest release. Unversioned development builds and
// installations with update checks disabled return an empty, unavailable
// result without network access. Source builds based on a release (for
// example v0.8.0.f1817c0c) are versioned development builds: they do check
// for, and can install, a newer release.
func Check(ctx context.Context, opts Options) (CheckResult, error) {
	result := CheckResult{CurrentVersion: opts.CurrentVersion}
	if isUnversionedDevelopmentVersion(opts.CurrentVersion) || os.Getenv("TANDEM_NO_UPDATE_CHECK") != "" {
		return result, nil
	}
	setDefaults(&opts)
	latest, err := fetchLatest(ctx, opts)
	if err != nil {
		return result, err
	}
	result.LatestVersion = latest.TagName
	result.Available, err = newerVersion(opts.CurrentVersion, latest.TagName)
	return result, err
}

// CheckAtStartup checks the latest GitHub release and reports how to update. It
// never prompts, downloads, or changes the running executable. Development
// builds and an explicitly disabled check do no network I/O.
func CheckAtStartup(ctx context.Context, opts Options) error {
	result, err := Check(ctx, opts)
	if err != nil {
		return err
	}
	if !result.Available {
		return nil
	}
	if opts.Log == nil {
		opts.Log = os.Stderr
	}
	fmt.Fprintf(opts.Log, "tandem: a newer release is available: %s (running %s); run 'tandem update' to install it\n", result.LatestVersion, result.CurrentVersion)
	return nil
}

// Update downloads the latest GitHub release, verifies its checksum, and
// atomically replaces the current executable. Invoking the explicit command is
// the user's consent, so this operation has no additional interactive prompt.
func Update(ctx context.Context, opts Options) error {
	_, err := UpdateWithResult(ctx, opts)
	return err
}

// UpdateWithResult behaves like Update and also reports whether it replaced the
// executable. Callers that need information provided only by the new binary
// can use the result to avoid querying it when no update was installed.
func UpdateWithResult(ctx context.Context, opts Options) (bool, error) {
	if isUnversionedDevelopmentVersion(opts.CurrentVersion) {
		return false, errors.New("self-update is unavailable for development builds")
	}
	setDefaults(&opts)
	latest, err := fetchLatest(ctx, opts)
	if err != nil {
		return false, err
	}
	newer, err := newerVersion(opts.CurrentVersion, latest.TagName)
	if err != nil {
		return false, err
	}
	if !newer {
		fmt.Fprintf(opts.Log, "tandem: already up to date (%s)\n", opts.CurrentVersion)
		return false, nil
	}
	binaryName := fmt.Sprintf("tandem_%s_%s", opts.GOOS, opts.GOARCH)
	binary, ok := findAsset(latest.Assets, binaryName)
	if !ok {
		return false, fmt.Errorf("release %s has no asset %s", latest.TagName, binaryName)
	}
	checksum, ok := findAsset(latest.Assets, binaryName+".sha256")
	if !ok {
		return false, fmt.Errorf("release %s has no checksum for %s", latest.TagName, binaryName)
	}
	wantSHA, err := downloadChecksum(ctx, opts.HTTPClient, checksum.DownloadURL)
	if err != nil {
		return false, err
	}
	if err := replaceExecutable(ctx, opts, binary.DownloadURL, wantSHA); err != nil {
		return false, err
	}
	if err := writeSelfUpdateMarker(opts.Executable, opts.CurrentVersion, latest.TagName, wantSHA); err != nil {
		slog.Error("self-update handoff failed", "executable", opts.Executable, "from_version", opts.CurrentVersion, "to_version", latest.TagName, "error", err)
		return false, err
	}
	slog.Info("self-update binary installed", "executable", opts.Executable, "from_version", opts.CurrentVersion, "to_version", latest.TagName)
	fmt.Fprintf(opts.Log, "tandem: updated %s from %s to %s\n", opts.Executable, opts.CurrentVersion, latest.TagName)
	return true, nil
}

// writeSelfUpdateMarker tells a source checkout launcher that the executable
// was deliberately replaced by a verified release. The launcher keeps using
// that release while its source revision is unchanged, instead of immediately
// rebuilding the old source over it on restart.
func writeSelfUpdateMarker(executable, previousVersion, installedVersion, sha string) error {
	target, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return fmt.Errorf("resolve updated executable for handoff: %w", err)
	}
	for name, value := range map[string]string{"previous version": previousVersion, "installed version": installedVersion, "checksum": sha} {
		if value == "" || strings.ContainsAny(value, "\t\r\n") {
			return fmt.Errorf("record self-update handoff: invalid %s", name)
		}
	}
	marker := target + selfUpdateMarkerSuffix
	temp, err := os.CreateTemp(filepath.Dir(marker), ".tandem-self-update-*")
	if err != nil {
		return fmt.Errorf("create self-update handoff: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("secure self-update handoff: %w", err)
	}
	if _, err := fmt.Fprintf(temp, "%s\t%s\t%s\n", installedVersion, previousVersion, strings.ToLower(sha)); err != nil {
		temp.Close()
		return fmt.Errorf("write self-update handoff: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync self-update handoff: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close self-update handoff: %w", err)
	}
	if err := os.Rename(tempName, marker); err != nil {
		return fmt.Errorf("publish self-update handoff: %w", err)
	}
	return nil
}

// isUnversionedDevelopmentVersion identifies builds for which no release
// lineage is known. A development build stamped with its nearest release and
// commit is safe to compare with releases and to replace atomically.
func isUnversionedDevelopmentVersion(value string) bool {
	return value == "" || value == "dev"
}

func setDefaults(opts *Options) {
	if opts.Repository == "" {
		opts.Repository = defaultRepository
	}
	if opts.APIBaseURL == "" {
		opts.APIBaseURL = "https://api.github.com"
	}
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
	}
	if opts.GOARCH == "" {
		opts.GOARCH = runtime.GOARCH
	}
	if opts.Log == nil {
		opts.Log = os.Stderr
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = newHTTPClient()
	}
	if opts.Executable == "" {
		executable, err := os.Executable()
		if err == nil {
			opts.Executable = executable
		}
	}
}

// newHTTPClient bounds establishing an update request without imposing a
// deadline on its body. Release binaries can take longer than the connection
// setup budget to download on a slow link, and an http.Client Timeout covers
// the entire response body.
func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: updateSetupTimeout, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = updateSetupTimeout
	transport.ResponseHeaderTimeout = updateSetupTimeout
	return &http.Client{Transport: transport}
}

func fetchLatest(ctx context.Context, opts Options) (release, error) {
	url := strings.TrimRight(opts.APIBaseURL, "/") + "/repos/" + opts.Repository + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return release{}, fmt.Errorf("create update request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "tandem/"+opts.CurrentVersion)
	response, err := opts.HTTPClient.Do(req)
	if err != nil {
		return release{}, fmt.Errorf("check for updates: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return release{}, fmt.Errorf("check for updates: GitHub returned %s", response.Status)
	}
	var latest release
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&latest); err != nil {
		return release{}, fmt.Errorf("decode latest release: %w", err)
	}
	return latest, nil
}

func findAsset(assets []asset, name string) (asset, bool) {
	for _, candidate := range assets {
		if candidate.Name == name {
			return candidate, true
		}
	}
	return asset{}, false
}

func downloadChecksum(ctx context.Context, client *http.Client, url string) (string, error) {
	data, err := download(ctx, client, url, 4096)
	if err != nil {
		return "", fmt.Errorf("download checksum: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 || len(fields[0]) != sha256.Size*2 {
		return "", errors.New("download checksum: invalid SHA-256 file")
	}
	if _, err := hex.DecodeString(fields[0]); err != nil {
		return "", errors.New("download checksum: invalid SHA-256 file")
	}
	return strings.ToLower(fields[0]), nil
}

func replaceExecutable(ctx context.Context, opts Options, url, wantSHA string) error {
	if opts.Executable == "" {
		return errors.New("locate current executable: path is empty")
	}
	target, err := filepath.EvalSymlinks(opts.Executable)
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}
	data, err := download(ctx, opts.HTTPClient, url, 256<<20)
	if err != nil {
		return fmt.Errorf("download update: %w", err)
	}
	gotSHA := fmt.Sprintf("%x", sha256.Sum256(data))
	if gotSHA != wantSHA {
		return fmt.Errorf("verify update: SHA-256 mismatch (got %s, want %s)", gotSHA, wantSHA)
	}

	temp, err := os.CreateTemp(filepath.Dir(target), ".tandem-update-*")
	if err != nil {
		return fmt.Errorf("create update beside executable: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write update: %w", err)
	}
	if err := temp.Chmod(0o755); err != nil {
		temp.Close()
		return fmt.Errorf("make update executable: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync update: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close update: %w", err)
	}
	if err := os.Rename(tempName, target); err != nil {
		return fmt.Errorf("replace %s: %w", target, err)
	}
	return nil
}

func download(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("response is too large")
	}
	return data, nil
}

func newerVersion(current, latest string) (bool, error) {
	currentVersion, err := parseVersion(current)
	if err != nil {
		return false, fmt.Errorf("compare current version: %w", err)
	}
	latestVersion, err := parseVersion(latest)
	if err != nil {
		return false, fmt.Errorf("compare latest version: %w", err)
	}
	return currentVersion.compare(latestVersion) < 0, nil
}

type version struct {
	major, minor, patch uint64
	prerelease          []string
	development         bool
}

func parseVersion(value string) (version, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	value = strings.SplitN(value, "+", 2)[0]
	// Local builds are stamped as vX.Y.Z.<eight-hex-commit>. They are commits
	// after their nearest release, so retain that distinction while parsing the
	// release portion. This intentionally gives the local build precedence over
	// the matching release, while a later semantic version still wins.
	development := developmentVersion.MatchString(value)
	if development {
		value = value[:strings.LastIndex(value, ".")]
	}
	parts := strings.SplitN(value, "-", 2)
	numbers := strings.Split(parts[0], ".")
	if len(numbers) != 3 {
		return version{}, fmt.Errorf("%q is not a semantic version", value)
	}
	parsed := version{development: development}
	values := []*uint64{&parsed.major, &parsed.minor, &parsed.patch}
	for index, number := range numbers {
		if number == "" || (len(number) > 1 && number[0] == '0') {
			return version{}, fmt.Errorf("%q is not a semantic version", value)
		}
		result, err := strconv.ParseUint(number, 10, 64)
		if err != nil {
			return version{}, fmt.Errorf("%q is not a semantic version", value)
		}
		*values[index] = result
	}
	if len(parts) == 2 {
		parsed.prerelease = strings.Split(parts[1], ".")
		for _, identifier := range parsed.prerelease {
			if identifier == "" {
				return version{}, fmt.Errorf("%q is not a semantic version", value)
			}
		}
	}
	return parsed, nil
}

func (v version) compare(other version) int {
	for _, pair := range [][2]uint64{{v.major, other.major}, {v.minor, other.minor}, {v.patch, other.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if len(v.prerelease) == 0 && len(other.prerelease) > 0 {
		return 1
	}
	if len(v.prerelease) > 0 && len(other.prerelease) == 0 {
		return -1
	}
	for index := 0; index < len(v.prerelease) && index < len(other.prerelease); index++ {
		left, right := v.prerelease[index], other.prerelease[index]
		if left == right {
			continue
		}
		leftNumber, leftErr := strconv.ParseUint(left, 10, 64)
		rightNumber, rightErr := strconv.ParseUint(right, 10, 64)
		if leftErr == nil && rightErr == nil {
			if leftNumber < rightNumber {
				return -1
			}
			return 1
		}
		if leftErr == nil {
			return -1
		}
		if rightErr == nil {
			return 1
		}
		if left < right {
			return -1
		}
		return 1
	}
	if len(v.prerelease) < len(other.prerelease) {
		return -1
	}
	if len(v.prerelease) > len(other.prerelease) {
		return 1
	}
	if v.development && !other.development {
		return 1
	}
	if !v.development && other.development {
		return -1
	}
	return 0
}
