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
//
// tandemBin is the path of the running tandem binary; the agent's PATH often
// lacks it. The prompt also spells out the clone's post-publish bookkeeping, so
// the agent finishes it rather than reporting it back as follow-up work.
func RebasePrompt(u runtimeinstall.UpdateInfo, clone, tandemBin string) string {
	fork := u.Fork
	if tandemBin == "" {
		tandemBin = "tandem"
	}
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
	fmt.Fprintf(&b, "\nThe `tandem` CLI may not be on your PATH; invoke it as `%s`.\n", tandemBin)

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

    ` + tandemBin + ` acp upstream ` + u.Agent + ` --constraint '^<version-that-has-it>'

That installs the published package, drops the fork tracking from Tandem's
config, and stops these notifications. Report what you verified and stop — do
not rebase.

## Step 2 — otherwise, rebase

Your worktree is on its own branch, and the clone itself has ` + "`" + fork.Ref + "`" + `
checked out, so do not try to check out ` + "`" + fork.Ref + "`" + ` here. Rebuild the fork
branch on this worktree's branch instead:

    git fetch origin && git fetch upstream
    git reset --hard origin/` + fork.Ref + `
    git rebase upstream/main

Resolve conflicts with a bias toward upstream's structure: the fork's job is to
be a small, re-appliable delta, so prefer reshaping the fork's change to fit
refactored upstream code over reverting upstream's refactor.

Then run the fork's own checks — do not skip these, a fork that builds but
misbehaves is worse than a stale one:

    npm ci    # or npm install if there is no lockfile
    npm run typecheck && npm run lint && npm test

Also confirm the packaging invariants Tandem depends on, which a rebase can
quietly break:

- ` + "`package.json`" + ` still has the same package ` + "`name`" + ` as upstream (Tandem
  resolves the entry point by that path).
- ` + "`npm run build`" + ` still produces the ACP entry point, and a ` + "`prepare`" + ` script
  still runs it, since Tandem installs this fork straight from a git SHA.

Keep the diff to what the rebase needs. Do not run formatters or other
whole-tree rewrites over commits you did not otherwise have to touch.

Update ` + "`FORK.md`" + ` for the new baseline (carried commits, what upstream now
covers, what would let the fork retire) and commit it.

## Step 3 — publish, re-pin, and sync the clone

Before pushing, ` + "`git status`" + ` must be clean: commit every intended change,
and discard incidental churn (lockfile rewrites from installing, formatter
output, build artifacts) with ` + "`git checkout -- <path>`" + ` / ` + "`git clean`" + `.

Push the rebased branch and record the new baseline so Tandem installs it and
stops flagging this release:

    git push --force-with-lease=` + fork.Ref + `:origin/` + fork.Ref + ` origin HEAD:` + fork.Ref + `
    ` + tandemBin + ` acp fork ` + u.Agent + ` --commit $(git rev-parse HEAD) --rebased-onto ` + u.LatestVersion + `

Bring the recorded upstream PRs up to date in the same command or a follow-up
one: pass ` + "`--pr N`" + ` once for each upstream PR that is still open and would
still retire the fork (this replaces the list), or ` + "`--no-prs`" + ` if none remain.
If the fork's purpose changed, update ` + "`--reason`" + ` too.

Then bring the clone's local branches up to the published state, since the
clone is what the next rebase starts from:

    git -C ` + clone + ` fetch origin
    git -C ` + clone + ` status --porcelain    # must print nothing
    git -C ` + clone + ` reset --hard origin/` + fork.Ref + `
    git branch -f main upstream/main          # if the clone has a local main

The clone is a Tandem-managed checkout, so resetting it is expected. Only if
` + "`status --porcelain`" + ` shows changes you did not make, leave it and report them.

Run ` + "`" + tandemBin + " acp status`" + ` to confirm the record reads as expected. The new
distribution is built on the next session spawn for this agent.

Finish with your worktree clean (` + "`git status`" + ` prints nothing to commit).

## Reporting

Say plainly which path you took and why. Do every step above yourself rather
than listing it as follow-up work; only report leftovers you genuinely could
not do, and say why. If you could not finish — an irreconcilable conflict, or
failing tests you could not fix — stop and report that, leaving the existing pin
in place. A stale-but-working fork is much better than a broken pin, and Tandem
will keep offering the rebase.
`)
	return b.String()
}
