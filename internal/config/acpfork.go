package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ACPFork records that one managed ACP server is installed from a fork rather
// than from its published npm package.
//
// Tandem carries a fork when an ACP server is missing something Tandem needs
// and upstream has not taken the change. The fork is a liability, not a goal:
// every field here exists so the daemon can keep pointing at upstream — telling
// the operator when a new upstream release appears, and handing an agent enough
// context to rebase onto it or to retire the fork once the change is upstreamed.
type ACPFork struct {
	// Repo is the fork's git remote, e.g. https://github.com/me/pi-acp. Tandem
	// installs from it with `npm install git+<Repo>#<Commit>`.
	Repo string `yaml:"repo" json:"repo"`
	// Ref is the branch carrying the fork's commits. It is what a rebase agent
	// checks out and force-pushes; Commit is what the daemon actually installs.
	Ref string `yaml:"ref" json:"ref"`
	// Commit pins the exact installed tree. A branch name alone would make
	// installs silently non-reproducible and would defeat the per-version
	// immutable distribution directories the runtime relies on.
	Commit string `yaml:"commit" json:"commit"`
	// UpstreamPackage is the npm package the fork is derived from, and the one
	// Tandem watches for new releases.
	UpstreamPackage string `yaml:"upstreamPackage" json:"upstreamPackage"`
	// UpstreamVersion is the published version the fork is currently rebased
	// onto. A newer release on npm is what triggers a rebase offer, so this is
	// the baseline a rebase agent must advance via `tandem acp fork --rebased-onto`.
	UpstreamVersion string `yaml:"upstreamVersion" json:"upstreamVersion"`
	// UpstreamRepo is the fork's source repository, used to check whether the
	// carried change has been upstreamed. Defaults to Repo's parent when empty.
	UpstreamRepo string `yaml:"upstreamRepo,omitempty" json:"upstreamRepo,omitempty"`
	// Reason states what the fork adds, in one line. It is shown in the ACP
	// manager and to a rebase agent, which needs it to judge whether upstream
	// has since covered the same ground.
	Reason string `yaml:"reason,omitempty" json:"reason,omitempty"`
	// UpstreamPRs lists pull requests that would make the fork unnecessary. A
	// rebase agent checks these first: a merged one means retire, not rebase.
	UpstreamPRs []int `yaml:"upstreamPrs,omitempty" json:"upstreamPrs,omitempty"`
	// Clone overrides where the working clone lives. Defaults to
	// <RuntimeRoot>/forks/<agent>.
	Clone string `yaml:"clone,omitempty" json:"clone,omitempty"`
}

// ACPForksKey is the top-level config.yml key holding the fork records.
const ACPForksKey = "acpForks"

var commitPattern = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// ForkCloneDir resolves where a fork-tracked agent's working clone lives. This
// is the repository a rebase agent is spawned against, so it must be a real
// clone with an `upstream` remote, not the installed distribution.
func ForkCloneDir(runtimeRoot, agent string, fork ACPFork) string {
	if strings.TrimSpace(fork.Clone) != "" {
		return fork.Clone
	}
	return filepath.Join(runtimeRoot, "forks", agent)
}

// InstallSpec is the npm argument that installs this fork's pinned commit.
func (f ACPFork) InstallSpec() string {
	repo := strings.TrimSuffix(strings.TrimSuffix(f.Repo, "/"), ".git")
	return "git+" + repo + ".git#" + f.Commit
}

// ShortCommit is the commit abbreviation used in distribution directory names
// and in operator-facing output.
func (f ACPFork) ShortCommit() string {
	if len(f.Commit) <= 12 {
		return f.Commit
	}
	return f.Commit[:12]
}

// DistributionVersion is the version string recorded in the agent lockfile for
// a fork install. It deliberately keeps the upstream version legible and marks
// the fork inline rather than inventing a synthetic semver: the runtime sorts
// and compares plain semvers elsewhere, and a fork must never look like a
// published release that npm could resolve.
func (f ACPFork) DistributionVersion() string {
	return f.UpstreamVersion + "+fork." + f.ShortCommit()
}

// IsForkVersion reports whether a recorded version came from a fork install.
func IsForkVersion(version string) bool { return strings.Contains(version, "+fork.") }

// Validate rejects a fork record the daemon could not act on.
func (f ACPFork) Validate() error {
	if strings.TrimSpace(f.Repo) == "" {
		return errors.New("fork repo is required")
	}
	if !strings.HasPrefix(f.Repo, "https://") && !strings.HasPrefix(f.Repo, "http://") {
		// git+ssh would need credentials the daemon may not have; an https
		// remote is what `npm install` can always reach unattended.
		return fmt.Errorf("fork repo must be an http(s) git URL, got %q", f.Repo)
	}
	if strings.TrimSpace(f.Ref) == "" {
		return errors.New("fork ref is required")
	}
	if !commitPattern.MatchString(f.Commit) {
		return fmt.Errorf("fork commit must be a hex git sha, got %q", f.Commit)
	}
	if strings.TrimSpace(f.UpstreamPackage) == "" {
		return errors.New("fork upstreamPackage is required")
	}
	if strings.TrimSpace(f.UpstreamVersion) == "" {
		return errors.New("fork upstreamVersion is required")
	}
	return nil
}

// LoadACPForks reads the fork records from config.yml, returning an empty map
// when the file or the block is absent. Like LoadMCPServers it reads the file on
// each call so `tandem acp` takes effect for new sessions without a daemon
// restart.
func LoadACPForks(home string) (map[string]ACPFork, error) {
	forks := map[string]ACPFork{}
	b, err := os.ReadFile(ConfigFilePath(home))
	if errors.Is(err, fs.ErrNotExist) {
		return forks, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		ACPForks map[string]ACPFork `yaml:"acpForks"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", ConfigFilePath(home), err)
	}
	for agent, fork := range doc.ACPForks {
		if strings.TrimSpace(agent) == "" {
			return nil, fmt.Errorf("invalid %s: %s keys must be agent ids", ConfigFilePath(home), ACPForksKey)
		}
		if err := fork.Validate(); err != nil {
			return nil, fmt.Errorf("invalid %s: %s.%s: %w", ConfigFilePath(home), ACPForksKey, agent, err)
		}
		forks[agent] = fork
	}
	return forks, nil
}

// SaveACPFork writes one fork record, preserving every unrelated key in
// config.yml.
func SaveACPFork(home, agent string, fork ACPFork) (string, error) {
	if strings.TrimSpace(agent) == "" {
		return "", errors.New("agent id is required")
	}
	if err := fork.Validate(); err != nil {
		return "", err
	}
	return mutateACPForks(home, func(forks map[string]ACPFork) error {
		forks[agent] = fork
		return nil
	})
}

// DeleteACPFork removes one fork record, returning whether it was present. This
// is how an agent returns a server to its published upstream package once the
// fork's change has landed there.
func DeleteACPFork(home, agent string) (string, bool, error) {
	existed := false
	path, err := mutateACPForks(home, func(forks map[string]ACPFork) error {
		_, existed = forks[agent]
		delete(forks, agent)
		return nil
	})
	return path, existed, err
}

func mutateACPForks(home string, apply func(map[string]ACPFork) error) (string, error) {
	path := ConfigFilePath(home)
	doc := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return "", fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	forks := map[string]ACPFork{}
	if raw, ok := doc[ACPForksKey]; ok {
		encoded, err := yaml.Marshal(raw)
		if err != nil || yaml.Unmarshal(encoded, &forks) != nil {
			return "", fmt.Errorf("parse %s: %s must be a mapping", path, ACPForksKey)
		}
	}
	if err := apply(forks); err != nil {
		return "", err
	}
	if len(forks) == 0 {
		delete(doc, ACPForksKey)
	} else {
		doc[ACPForksKey] = forks
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return "", err
	}
	return path, os.Chmod(path, 0o600)
}
