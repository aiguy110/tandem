# Repo-local skills

When a user asks for a repo-local skill, keep one canonical copy at
`skills/<skill-name>/SKILL.md`. Do not make separate copies per agent.

Expose that directory with these project-local bridges when they are absent:

- Codex: `.agents/skills` → `../skills`
- Claude Code: `.claude/skills` → `../skills`
- Pi: `.pi/settings.json` with `"skills": ["../skills"]`

Preserve an existing bridge or Pi settings file; merge its configuration rather
than overwriting it. A skill directory may include optional `agents/`,
`scripts/`, `references/`, and `assets/` alongside `SKILL.md`.

For Tandem's generated home-base implementation, see
[`internal/homebase`](../internal/homebase/homebase.go).
