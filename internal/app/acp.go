package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/runtimeinstall"
)

const acpUsage = `usage: tandem acp <command>

  status [--json]                      show managed ACP servers and fork tracking
  fork AGENT [flags]                   create or update fork tracking for AGENT
  upstream AGENT [--constraint RANGE]  retire AGENT's fork, returning to npm
  sync AGENT [--check]                 adopt the tree published on AGENT's fork
                                       branch (or retire, if it says so)
  manifest AGENT [fork flags] [--retired-for RANGE] [--write PATH]
                                       print the tandem-fork.json manifest for
                                       AGENT's record with the flags applied

fork flags:
  --repo URL            fork git remote (https)
  --ref REF             branch carrying the fork's commits
  --commit SHA          exact commit to install; use $(git rev-parse HEAD)
  --upstream-package P  npm package the fork derives from
  --rebased-onto VER    published version the fork now sits on
  --upstream-repo URL   source repository, for upstreaming checks
  --reason TEXT         one line on what the fork adds
  --pr N                upstream PR that would retire the fork (repeatable;
                        replaces the recorded list)
  --no-prs              clear the recorded upstream PR list
  --package-dir DIR     package path inside a monorepo, e.g. packages/coding-agent
                        (bun install runs at the repo root, build and pack here;
                        "" resets to the repo root)
  --build-script NAME   script run before packing; "none" relies on the pack's
                        prepack lifecycle; "" resets to build, else prepare
  --follow POLICY       newer trees published on the fork branch: offer
                        (default), auto (adopt unprompted), or off

On an existing record every flag is optional and only the given fields change,
so recording a completed rebase is just:

  tandem acp fork AGENT --commit $(git rev-parse HEAD) --rebased-onto VERSION

Sharing a rebase: the fork branch is shared by every Tandem tracking it. Commit
the manifest at the branch tip so other instances adopt the tree instead of
redoing the rebase, then pin it here from the branch:

  tandem acp manifest AGENT --rebased-onto VERSION --write tandem-fork.json
  git add tandem-fork.json && git commit -m "tandem-fork: VERSION" && git push
  tandem acp sync AGENT`

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
	case "sync":
		return acpSync(args[1:], stdout, stderr)
	case "manifest":
		return acpManifest(args[1:], stdout, stderr)
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
		if f.PackageDir != "" || f.BuildScript != "" {
			fmt.Fprintf(stdout, "         package dir: %s, build script: %s\n", orDefault(f.PackageDir, "(repo root)"), orDefault(f.BuildScript, "(build, else prepare)"))
		}
		fmt.Fprintf(stdout, "         published trees on %s: %s\n", f.Ref, orDefault(f.Follow, config.ForkFollowOffer))
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

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// forkEdits is a parsed set of `tandem acp fork` flags. Only fields named on
// the command line are changed, so an agent recording a rebase does not have
// to restate the whole record.
type forkEdits struct {
	set        map[string]string
	prs        []int
	prsGiven   bool
	retiredFor string
	write      string
}

// parseForkEdits reads fork flags; manifest additionally accepts
// --retired-for and --write.
func parseForkEdits(args []string, manifest bool, stderr io.Writer) (forkEdits, bool) {
	e := forkEdits{set: map[string]string{}}
	for len(args) > 0 {
		flag := args[0]
		switch flag {
		case "--repo", "--ref", "--commit", "--upstream-package", "--rebased-onto", "--upstream-repo", "--reason", "--clone", "--package-dir", "--build-script", "--follow":
			if len(args) < 2 {
				fmt.Fprintf(stderr, "acp: %s requires a value\n", flag)
				return e, false
			}
			e.set[flag], args = args[1], args[2:]
		case "--pr":
			if len(args) < 2 {
				fmt.Fprintf(stderr, "acp: %s requires a value\n", flag)
				return e, false
			}
			n, err := strconv.Atoi(args[1])
			if err != nil {
				fmt.Fprintf(stderr, "acp: --pr must be a number, got %q\n", args[1])
				return e, false
			}
			e.prs, e.prsGiven, args = append(e.prs, n), true, args[2:]
		case "--no-prs":
			e.prs, e.prsGiven, args = nil, true, args[1:]
		case "--retired-for", "--write":
			if !manifest {
				fmt.Fprintf(stderr, "acp: %s is a manifest flag\n%s\n", flag, acpUsage)
				return e, false
			}
			if len(args) < 2 {
				fmt.Fprintf(stderr, "acp: %s requires a value\n", flag)
				return e, false
			}
			if flag == "--write" {
				e.write = args[1]
			} else {
				e.retiredFor = strings.TrimSpace(args[1])
			}
			args = args[2:]
		default:
			fmt.Fprintf(stderr, "acp: unknown fork flag %q\n%s\n", flag, acpUsage)
			return e, false
		}
	}
	return e, true
}

func (e forkEdits) apply(fork config.ACPFork) config.ACPFork {
	for flag, value := range e.set {
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
		case "--package-dir":
			fork.PackageDir = value
		case "--build-script":
			fork.BuildScript = value
		case "--follow":
			fork.Follow = value
		}
	}
	if e.prsGiven {
		fork.UpstreamPRs = e.prs
	}
	return fork
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
	edits, ok := parseForkEdits(args, false, stderr)
	if !ok {
		return 2
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
	fork = edits.apply(fork)
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

// acpManifest prints (or writes) the blessed-fork manifest for an agent's
// record with the given edits applied, without changing the record. A rebase
// agent commits it at the fork branch tip, pushes, and then pins the result
// with `sync`, so the record and the branch cannot disagree.
func acpManifest(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "--") {
		fmt.Fprintln(stderr, "acp: manifest requires an agent id first")
		return 2
	}
	agent, args := args[0], args[1:]
	edits, ok := parseForkEdits(args, true, stderr)
	if !ok {
		return 2
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
	fork, tracked := forks[agent]
	if !tracked {
		fmt.Fprintf(stderr, "acp: agent %s is not tracking a fork\n", agent)
		return 1
	}
	fork = edits.apply(fork)
	if err := fork.Validate(); err != nil {
		fmt.Fprintf(stderr, "acp: %v\n", err)
		return 1
	}
	m := runtimeinstall.ManifestFor(fork)
	m.RetiredFor = edits.retiredFor
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "acp: encode manifest: %v\n", err)
		return 1
	}
	out = append(out, '\n')
	if edits.write == "" {
		_, _ = stdout.Write(out)
		return 0
	}
	if err := os.WriteFile(edits.write, out, 0o644); err != nil {
		fmt.Fprintf(stderr, "acp: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s for %s (rebased onto %s@%s)\n", edits.write, agent, m.UpstreamPackage, m.UpstreamVersion)
	return 0
}

// acpSync follows the fork branch: it re-pins to the tree published at its tip
// or retires the fork when the tip's manifest says so.
func acpSync(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "--") {
		fmt.Fprintln(stderr, "acp: sync requires an agent id first")
		return 2
	}
	agent, check := args[0], false
	for _, arg := range args[1:] {
		if arg != "--check" {
			fmt.Fprintf(stderr, "acp: unknown sync flag %q\n%s\n", arg, acpUsage)
			return 2
		}
		check = true
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "acp: load config: %v\n", err)
		return 1
	}
	ctx := context.Background()
	if check {
		fork, tracked, err := runtimeinstall.ForkFor(cfg, agent)
		if err != nil || !tracked {
			fmt.Fprintf(stderr, "acp: agent %s is not tracking a fork (%v)\n", agent, err)
			return 1
		}
		b, err := runtimeinstall.ReadBlessed(ctx, cfg, agent, fork)
		if err != nil {
			fmt.Fprintf(stderr, "acp: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s branch %s tip %s; pinned here %s (rebased onto %s)\n", fork.Repo, fork.Ref, b.ShortCommit(), fork.ShortCommit(), fork.UpstreamVersion)
		switch m := b.Manifest; {
		case m == nil:
			fmt.Fprintf(stdout, "tip has no %s; nothing published to sync\n", runtimeinstall.BlessedManifestFile)
		case m.RetiredFor != "":
			fmt.Fprintf(stdout, "tip says the fork is retired for %s@%s\n", m.UpstreamPackage, m.RetiredFor)
		default:
			fmt.Fprintf(stdout, "tip is rebased onto %s@%s\n", m.UpstreamPackage, m.UpstreamVersion)
		}
		return 0
	}
	res, err := runtimeinstall.SyncBlessed(ctx, cfg, agent, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "acp: %v\n", err)
		return 1
	}
	switch res.Kind {
	case runtimeinstall.UpdateKindAdopt:
		fmt.Fprintf(stdout, "%s now pinned to %s#%s, rebased onto %s@%s (was %s on %s); built and ready for new sessions\n", agent,
			res.After.Repo, res.After.ShortCommit(), res.After.UpstreamPackage, res.After.UpstreamVersion, res.Before.ShortCommit(), res.Before.UpstreamVersion)
	case runtimeinstall.UpdateKindRetire:
		fmt.Fprintf(stdout, "%s fork retired as its branch directs; now using the published %s@%s\n", agent, res.Locked.Package, res.Locked.Version)
	default:
		fmt.Fprintf(stdout, "%s already pinned to the branch tip %s\n", agent, res.Blessed.ShortCommit())
	}
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
