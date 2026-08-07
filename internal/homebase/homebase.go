// Package homebase scaffolds and maintains the per-installation "home base"
// repository at $TANDEM_HOME/home-base. The home base is the canonical,
// agent-navigable home for Tandem-specific knowledge and non-repo automation
// (scheduled scripts, personal connectors, self-evolving skills). It is a
// git repo so that self-evolving skills accumulate with reviewable history,
// and it is surfaced in the spawn palette via config.Config.HomeBaseDir.
//
// Ensure is idempotent and runs on every daemon start: it creates the repo on
// first boot and, on later boots, refreshes only the daemon-generated block of
// AGENTS.md (delimited by markers) so upgrades update the overview and links
// without clobbering anything a human or agent has authored around it.
package homebase

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aiguy110/tandem/internal/config"
)

// Markers delimiting the daemon-owned region of AGENTS.md. Everything outside
// them is authored (by a human or an agent) and never rewritten.
const (
	genBegin = "<!-- tandem:generated:begin — regenerated on daemon start; edit outside this block -->"
	genEnd   = "<!-- tandem:generated:end -->"
)

// sourceRepoURL is the canonical upstream, used for docs/source links when the
// deployed installation has no local source tree (a release-binary install).
const sourceRepoURL = "https://github.com/aiguy110/tandem"

// Ensure scaffolds the home base if absent and refreshes its generated content.
// Failures are returned but treated as non-fatal by the caller: the daemon must
// still serve even if scaffolding hits a snag.
func Ensure(ctx context.Context, cfg config.Config, out io.Writer) error {
	dir := cfg.HomeBaseDir
	if dir == "" {
		return nil
	}
	created, err := ensureDir(dir)
	if err != nil {
		return fmt.Errorf("create home base dir: %w", err)
	}

	fresh := !isGitRepo(dir)
	if fresh {
		if err := git(ctx, dir, "init"); err != nil {
			// A missing git binary should not block the daemon; skip the repo
			// niceties but still lay down the files below.
			fmt.Fprintf(out, "tandem: home base git init failed (%v); scaffolding files without a repo\n", err)
			fresh = false
		}
	}

	if err := writeAgentsFile(dir, cfg); err != nil {
		return err
	}
	if err := ensureClaudeSymlink(dir); err != nil {
		return err
	}
	if err := ensureStarterTree(dir); err != nil {
		return err
	}

	if created {
		fmt.Fprintf(out, "tandem: scaffolded home base at %s\n", dir)
	}
	// Only auto-commit on first creation. Later regenerations are left as
	// working-tree changes so a human/agent reviews them — consistent with the
	// "agents propose, humans approve" posture of the automation grant model.
	if fresh {
		_ = git(ctx, dir, "add", "-A")
		if err := commit(ctx, dir, "Initialize Tandem home base"); err != nil {
			fmt.Fprintf(out, "tandem: home base initial commit skipped (%v)\n", err)
		}
	}
	return nil
}

// ensureDir creates dir if needed, reporting whether it was newly created.
func ensureDir(dir string) (bool, error) {
	if _, err := os.Stat(dir); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	return true, nil
}

func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// writeAgentsFile creates AGENTS.md with a generated block plus an authored
// starter section on first run; on later runs it replaces only the generated
// block, preserving everything a human/agent added around it. If the markers
// have been removed from an existing file, the file is left untouched.
func writeAgentsFile(dir string, cfg config.Config) error {
	path := filepath.Join(dir, "AGENTS.md")
	generated := renderGenerated(cfg)

	existing, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		content := generated + "\n\n" + authoredStarter
		return os.WriteFile(path, []byte(content), 0o644)
	}

	text := string(existing)
	start := strings.Index(text, genBegin)
	end := strings.Index(text, genEnd)
	if start == -1 || end == -1 || end < start {
		// Authored file without our markers — respect it, change nothing.
		return nil
	}
	replaced := text[:start] + generated + text[end+len(genEnd):]
	if replaced == text {
		return nil
	}
	return os.WriteFile(path, []byte(replaced), 0o644)
}

// renderGenerated builds the daemon-owned block, resolving docs/source links to
// the local tree when this installation has one and always including upstream.
func renderGenerated(cfg config.Config) string {
	var b strings.Builder
	b.WriteString(genBegin)
	b.WriteString("\n\n# Tandem home base\n\n")
	b.WriteString("This is the home base repo for this Tandem installation. Tandem is a ")
	b.WriteString("browser-based orchestration layer for terminal coding agents: a self-hosted ")
	b.WriteString("daemon owns all state, and agents run in isolated git worktrees. This repo is ")
	b.WriteString("the canonical, editable home for Tandem-specific knowledge and for automation ")
	b.WriteString("that isn't tied to any product repo — scheduled scripts, personal connectors, ")
	b.WriteString("and self-evolving skills.\n\n")

	b.WriteString("## This installation\n\n")
	if localDocs := localDocsDir(cfg); localDocs != "" {
		fmt.Fprintf(&b, "- Docs (local): `%s`\n", localDocs)
	}
	if cfg.TandemRoot != "" {
		fmt.Fprintf(&b, "- Source (local): `%s`\n", cfg.TandemRoot)
	}
	fmt.Fprintf(&b, "- Source & docs (upstream): %s\n", sourceRepoURL)
	fmt.Fprintf(&b, "- Daemon home (`TANDEM_HOME`): `%s`\n\n", cfg.Home)

	b.WriteString("## Scripting / automation framework\n\n")
	b.WriteString("Repository-owned TypeScript automations live under `.tandem/scripts/**/*.ts`. ")
	b.WriteString("They run as ordinary Node processes with full host access, but reach Tandem ")
	b.WriteString("tools (browser, etc.) only through explicit, repository-scoped MCP grants. ")
	b.WriteString("Agents drive them with the `tandem-scripts` MCP tools: `scripts_run` (run a ")
	b.WriteString("saved script), `scripts_evaluate` (run ephemeral source), and ")
	b.WriteString("`scripts_preapprove` (request grants + optionally register a schedule). A ")
	b.WriteString("script frontmatter block (`@tandem`) declares its name, requested tools, ")
	b.WriteString("optional cron schedule, and how it wakes an agent on a condition of interest. ")
	b.WriteString("Adding a file never activates a schedule — registration is explicit.\n\n")

	b.WriteString("## Self-evolving skills\n\n")
	b.WriteString("`skills/` holds skills you (agents) create and refine over time — durable ")
	b.WriteString("guidance and automations that accumulate with git history so a bad change can ")
	b.WriteString("be reviewed and reverted. When you learn something reusable about operating ")
	b.WriteString("Tandem or the user's environment, capture it here.\n\n")

	b.WriteString("**Invariant:** a skill or script may *propose* new capabilities, but a human ")
	b.WriteString("approves any new MCP grant — agents can request grants, never self-approve.\n")
	b.WriteString("\n")
	b.WriteString(genEnd)
	return b.String()
}

// localDocsDir returns the installation's on-disk docs directory if the source
// tree is present (a dev/source install), else "".
func localDocsDir(cfg config.Config) string {
	if cfg.TandemRoot == "" {
		return ""
	}
	docs := filepath.Join(cfg.TandemRoot, "docs")
	if info, err := os.Stat(docs); err == nil && info.IsDir() {
		return docs
	}
	return ""
}

// authoredStarter is written once, below the generated block, and never touched
// again. It is the human/agent-owned surface of the file.
const authoredStarter = `## Notes

_Your notes, conventions, and links live here. Tandem never rewrites anything
below the generated block above._

## Skills index

- (none yet — see ` + "`skills/`" + `)
`

// ensureClaudeSymlink points CLAUDE.md at AGENTS.md so Claude-convention
// harnesses read the same canonical file. It never clobbers an existing entry.
func ensureClaudeSymlink(dir string) error {
	path := filepath.Join(dir, "CLAUDE.md")
	if _, err := os.Lstat(path); err == nil {
		return nil // already present (symlink or authored file) — leave it
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Symlink("AGENTS.md", path); err != nil {
		// Symlinks can fail (e.g. some filesystems/Windows); fall back to a
		// tiny pointer file rather than duplicating content.
		return os.WriteFile(path, []byte("See [AGENTS.md](AGENTS.md).\n"), 0o644)
	}
	return nil
}

// ensureStarterTree creates the skills/ and .tandem/scripts/ directories with a
// one-time README each, so agents land somewhere obvious. Existing files are
// left alone.
func ensureStarterTree(dir string) error {
	entries := map[string]string{
		filepath.Join("skills", "README.md"): "# Skills\n\n" +
			"Self-evolving skills for this Tandem installation. Each skill is a directory " +
			"with guidance (and optionally scripts under `.tandem/scripts/`). Create and " +
			"refine them as you learn; git history is the safety net.\n",
		filepath.Join(".tandem", "scripts", "README.md"): "# Automation scripts\n\n" +
			"TypeScript automations (`*.ts`) run via the `tandem-scripts` MCP tools. Declare " +
			"an `@tandem` frontmatter block for name, requested MCP tools, schedule, and wake " +
			"behavior. Adding a file does not activate its schedule — register it explicitly " +
			"with `scripts_preapprove`.\n",
	}
	for rel, body := range entries {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if _, err := os.Stat(full); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func isGitInstalled() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

func git(ctx context.Context, dir string, args ...string) error {
	if !isGitInstalled() {
		return fmt.Errorf("git not found in PATH")
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}

// commit commits with an identity override so a fresh machine without a global
// git user can still record the initial commit.
func commit(ctx context.Context, dir, message string) error {
	args := []string{
		"-c", "user.name=Tandem", "-c", "user.email=tandem@localhost",
		"commit", "-m", message,
	}
	return git(ctx, dir, args...)
}
