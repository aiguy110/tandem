# Automations

Use the `tandem-scripts` MCP for repository-owned TypeScript automation. Saved
scripts live under `.tandem/scripts/`; they can use ordinary Node APIs and
explicitly granted Tandem tools.

- `scripts_run` runs a saved script.
- `scripts_evaluate` runs ephemeral TypeScript.
- `scripts_preapprove` requests grants and may register a schedule.

Scripts cannot approve their own capabilities: the user must approve each new
repository-scoped MCP grant. Adding or editing a script never activates a
schedule by itself.

Read the [agent-facing API](../docs/scripting-framework.md#agent-facing-api)
for tool inputs, script frontmatter, scheduling, and wakeups.
