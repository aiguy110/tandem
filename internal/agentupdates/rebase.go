package agentupdates

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/aiguy110/tandem/internal/runtimeinstall"
)

// RebasePrompt is the task handed to an agent spawned to move a tracked ACP
// server fork onto a new upstream release.
//
// It leads with the retire-or-rebase decision on purpose. The whole point of
// tracking a fork is to stop carrying it as soon as upstream makes it
// unnecessary, and an agent that rebases by reflex would keep the fork alive
// long after the change landed upstream.
func RebasePrompt(u runtimeinstall.UpdateInfo, clone string) string {
	fork := u.Fork
	var b strings.Builder

	fmt.Fprintf(&b, "Tandem carries a fork of the `%s` ACP server (agent id `%s`), and upstream has published a new release. Decide whether to retire the fork or rebase it, then carry that out.\n\n", u.Package, u.Agent)

	b.WriteString("## The fork\n\n")
	fmt.Fprintf(&b, "- Working clone: `%s` (you have been spawned in a worktree of it)\n", clone)
	fmt.Fprintf(&b, "- Fork remote: %s, branch `%s`, currently pinned at `%s`\n", fork.Repo, fork.Ref, fork.Commit)
	if fork.UpstreamRepo != "" {
		fmt.Fprintf(&b, "- Upstream repo: %s (should be the `upstream` git remote)\n", fork.UpstreamRepo)
	}
	fmt.Fprintf(&b, "- Upstream npm package: `%s`\n", fork.UpstreamPackage)
	fmt.Fprintf(&b, "- Fork is rebased onto: **%s**. Newest published release: **%s**\n", u.CurrentVersion, u.LatestVersion)
	if fork.Reason != "" {
		fmt.Fprintf(&b, "- Why the fork exists: %s\n", fork.Reason)
	}
	if len(fork.UpstreamPRs) > 0 {
		prs := make([]string, 0, len(fork.UpstreamPRs))
		for _, n := range fork.UpstreamPRs {
			prs = append(prs, "#"+strconv.Itoa(n))
		}
		fmt.Fprintf(&b, "- Upstream PRs that would make the fork unnecessary: %s\n", strings.Join(prs, ", "))
	}
	b.WriteString("\nRead `FORK.md` in the clone if it exists; it should record exactly which commits are carried and why.\n")

	b.WriteString(`
## Step 1 — check whether the change has been upstreamed first

Do this before any rebase work. If upstream now does what the fork does, the
correct outcome is to **retire the fork**, not to rebase it.

    git fetch upstream

Then check, using the fork's reason above to know what to look for:

- Have any of the upstream PRs listed above been merged? (` + "`gh pr view <n> --repo <upstream> --json state,mergedAt`" + `)
- Does ` + "`upstream/main`" + ` already contain the behaviour? Diff the fork's carried
  commits against it rather than trusting the PR list — the change may have
  landed through a different PR, or been reimplemented differently.
- Read the upstream CHANGELOG for the new release.

**If it has been upstreamed**, verify it genuinely covers the fork's purpose
(equivalent behaviour, not just a similar-sounding entry), then retire the fork:

    tandem acp upstream ` + u.Agent + ` --constraint '^<version-that-has-it>'

That installs the published package, drops the fork tracking from Tandem's
config, and stops these notifications. Report what you verified and stop — do
not rebase.

## Step 2 — otherwise, rebase

    git fetch upstream
    git checkout main && git merge --ff-only upstream/main
    git checkout ` + fork.Ref + ` && git rebase main

Resolve conflicts with a bias toward upstream's structure: the fork's job is to
be a small, re-appliable delta, so prefer reshaping the fork's change to fit
refactored upstream code over reverting upstream's refactor.

Then run the fork's own checks — do not skip these, a fork that builds but
misbehaves is worse than a stale one:

    npm install
    npm run typecheck && npm run lint && npm test

Also confirm the packaging invariants Tandem depends on, which a rebase can
quietly break:

- ` + "`package.json`" + ` still has the same package ` + "`name`" + ` as upstream (Tandem
  resolves the entry point by that path).
- ` + "`npm run build`" + ` still produces the ACP entry point, and a ` + "`prepare`" + ` script
  still runs it, since Tandem installs this fork straight from a git SHA.

## Step 3 — publish and re-pin

Push the rebased branch, then record the new baseline so Tandem installs it and
stops flagging this release:

    git push --force-with-lease origin ` + fork.Ref + `
    tandem acp fork ` + u.Agent + ` --commit $(git rev-parse HEAD) --rebased-onto ` + u.LatestVersion + `

Run ` + "`tandem acp status`" + ` to confirm the record reads as expected. The new
distribution is built on the next session spawn for this agent.

## Reporting

Say plainly which path you took and why. If you could not finish — an
irreconcilable conflict, or failing tests you could not fix — stop and report
that, leaving the existing pin in place. A stale-but-working fork is much better
than a broken pin, and Tandem will keep offering the rebase.
`)
	return b.String()
}
