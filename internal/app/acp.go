package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/runtimeinstall"
)

const acpUsage = `usage: tandem acp <command>

  status [--json]                      show managed ACP servers and fork tracking
  fork AGENT [flags]                   create or update fork tracking for AGENT
  upstream AGENT [--constraint RANGE]  retire AGENT's fork, returning to npm

fork flags:
  --repo URL            fork git remote (https)
  --ref REF             branch carrying the fork's commits
  --commit SHA          exact commit to install; use $(git rev-parse HEAD)
  --upstream-package P  npm package the fork derives from
  --rebased-onto VER    published version the fork now sits on
  --upstream-repo URL   source repository, for upstreaming checks
  --reason TEXT         one line on what the fork adds
  --pr N                upstream PR that would retire the fork (repeatable)

On an existing record every flag is optional and only the given fields change,
so recording a completed rebase is just:

  tandem acp fork AGENT --commit $(git rev-parse HEAD) --rebased-onto VERSION`

// runACP dispatches `tandem acp`. It is the agent-facing surface of ACP fork
// tracking: an agent spawned to rebase a fork re-pins it with `fork`, or retires
// it with `upstream` when it finds the change was upstreamed.
func runACP(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, acpUsage)
		return 2
	}
	switch args[0] {
	case "status":
		return acpStatus(args[1:], stdout, stderr)
	case "fork":
		return acpFork(args[1:], stdout, stderr)
	case "upstream":
		return acpUpstream(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown acp command %q\n%s\n", args[0], acpUsage)
		return 2
	}
}

func acpStatus(args []string, stdout, stderr io.Writer) int {
	asJSON := false
	for _, arg := range args {
		if arg != "--json" {
			fmt.Fprintln(stderr, acpUsage)
			return 2
		}
		asJSON = true
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "acp: load config: %v\n", err)
		return 1
	}
	rows, err := runtimeinstall.AdapterCatalog(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(stderr, "acp: read adapter catalog: %v\n", err)
		return 1
	}
	if asJSON {
		out, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "acp: encode status: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(out))
		return 0
	}
	for _, row := range rows {
		if row.Fork == nil {
			fmt.Fprintf(stdout, "%-8s %s  current %s  latest %s\n", row.Agent, row.Package, orNone(row.CurrentVersion), orNone(row.LatestVersion))
			// Updates are offered optimistically, so say when the newest release is
			// outside the range Tandem has been tested against — that is the case an
			// operator most needs to know about before taking it.
			if row.LatestCompatibleVersion != "" {
				fmt.Fprintf(stdout, "         newest tested %s (range %s); %s is published but untested\n",
					row.LatestCompatibleVersion, row.Constraint, row.LatestVersion)
			}
			if row.CurrentVersion != "" && !slices.Contains(row.CompatibleVersions, row.CurrentVersion) {
				fmt.Fprintf(stdout, "         installed %s is beyond the tested range %s\n", row.CurrentVersion, row.Constraint)
			}
			continue
		}
		f := row.Fork
		fmt.Fprintf(stdout, "%-8s %s  FORK %s#%s\n", row.Agent, row.Package, f.Repo, f.ShortCommit)
		fmt.Fprintf(stdout, "         ref %s, rebased onto %s", f.Ref, f.UpstreamVersion)
		if f.LatestUpstreamVersion != "" {
			fmt.Fprintf(stdout, "  (upstream %s available — rebase or retire)", f.LatestUpstreamVersion)
		} else {
			fmt.Fprint(stdout, "  (level with upstream)")
		}
		fmt.Fprintln(stdout)
		if !f.Installed {
			fmt.Fprintln(stdout, "         pinned commit is not built yet; it installs on the next spawn of this agent")
		}
		if f.Reason != "" {
			fmt.Fprintf(stdout, "         reason: %s\n", f.Reason)
		}
		if len(f.UpstreamPRs) > 0 {
			prs := make([]string, 0, len(f.UpstreamPRs))
			for _, n := range f.UpstreamPRs {
				prs = append(prs, "#"+strconv.Itoa(n))
			}
			fmt.Fprintf(stdout, "         watching upstream PRs: %s\n", strings.Join(prs, ", "))
		}
		fmt.Fprintf(stdout, "         clone: %s\n", f.Clone)
	}
	return 0
}

func orNone(v string) string {
	if v == "" {
		return "(not installed)"
	}
	return v
}

func acpFork(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, acpUsage)
		return 2
	}
	agent, args := args[0], args[1:]
	if strings.HasPrefix(agent, "--") {
		fmt.Fprintln(stderr, "acp: fork requires an agent id first")
		return 2
	}
	// Only fields named on the command line are changed, so an agent recording a
	// rebase does not have to restate the whole record.
	set := map[string]string{}
	var prs []int
	prsGiven := false
	for len(args) > 0 {
		flag := args[0]
		switch flag {
		case "--repo", "--ref", "--commit", "--upstream-package", "--rebased-onto", "--upstream-repo", "--reason", "--clone":
			if len(args) < 2 {
				fmt.Fprintf(stderr, "acp: %s requires a value\n", flag)
				return 2
			}
			set[flag], args = args[1], args[2:]
		case "--pr":
			if len(args) < 2 {
				fmt.Fprintf(stderr, "acp: %s requires a value\n", flag)
				return 2
			}
			n, err := strconv.Atoi(args[1])
			if err != nil {
				fmt.Fprintf(stderr, "acp: --pr must be a number, got %q\n", args[1])
				return 2
			}
			prs, prsGiven, args = append(prs, n), true, args[2:]
		default:
			fmt.Fprintf(stderr, "acp: unknown fork flag %q\n%s\n", flag, acpUsage)
			return 2
		}
	}
	home, err := setupHome()
	if err != nil {
		fmt.Fprintf(stderr, "acp: resolve TANDEM_HOME: %v\n", err)
		return 1
	}
	forks, err := config.LoadACPForks(home)
	if err != nil {
		fmt.Fprintf(stderr, "acp: %v\n", err)
		return 1
	}
	fork, existed := forks[agent]
	for flag, value := range set {
		switch flag {
		case "--repo":
			fork.Repo = value
		case "--ref":
			fork.Ref = value
		case "--commit":
			fork.Commit = value
		case "--upstream-package":
			fork.UpstreamPackage = value
		case "--rebased-onto":
			fork.UpstreamVersion = value
		case "--upstream-repo":
			fork.UpstreamRepo = value
		case "--reason":
			fork.Reason = value
		case "--clone":
			fork.Clone = value
		}
	}
	if prsGiven {
		fork.UpstreamPRs = prs
	}
	if !existed {
		// A new record must be complete; defaulting a ref or a package would be
		// guessing at what tree to install.
		if fork.Ref == "" {
			fork.Ref = "tandem"
		}
		if fork.UpstreamPackage == "" {
			fmt.Fprintln(stderr, "acp: --upstream-package is required when first tracking a fork")
			return 2
		}
	}
	path, err := config.SaveACPFork(home, agent, fork)
	if err != nil {
		fmt.Fprintf(stderr, "acp: %v\n", err)
		return 1
	}
	verb := "updated"
	if !existed {
		verb = "now tracking"
	}
	fmt.Fprintf(stdout, "%s ACP fork for %s: %s#%s on %s, rebased onto %s@%s\n", verb, agent,
		fork.Repo, fork.ShortCommit(), fork.Ref, fork.UpstreamPackage, fork.UpstreamVersion)
	fmt.Fprintf(stdout, "recorded in %s; the pinned tree is installed on the next %s session spawn\n", path, agent)
	return 0
}

func acpUpstream(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, acpUsage)
		return 2
	}
	agent, args := args[0], args[1:]
	version := ""
	for len(args) > 0 {
		switch args[0] {
		case "--version", "--constraint":
			if len(args) < 2 {
				fmt.Fprintf(stderr, "acp: %s requires a value\n", args[0])
				return 2
			}
			// A range is accepted for symmetry with the docs and with how an agent
			// naturally thinks about "the release that has it"; npm resolves it.
			version, args = strings.TrimSpace(args[1]), args[2:]
		default:
			fmt.Fprintf(stderr, "acp: unknown upstream flag %q\n%s\n", args[0], acpUsage)
			return 2
		}
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "acp: load config: %v\n", err)
		return 1
	}
	if version != "" && strings.ContainsAny(version, "^~><= *x") {
		resolved, err := runtimeinstall.ResolveVersion(context.Background(), cfg, agent, version)
		if err != nil {
			fmt.Fprintf(stderr, "acp: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "resolved %s to %s\n", version, resolved)
		version = resolved
	}
	locked, err := runtimeinstall.ReturnToUpstream(context.Background(), cfg, agent, version, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "acp: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s now uses the published %s@%s; fork tracking removed. New sessions pick it up immediately.\n", agent, locked.Package, locked.Version)
	fmt.Fprintf(stdout, "the fork clone is left on disk; delete it once you are satisfied nothing regressed.\n")
	return 0
}
