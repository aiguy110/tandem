// The command registry: stable IDs → actions, the single source both the keymap
// and the command palette resolve against (docs/spawn-and-workspaces.md D10).

import { useStore, allApprovals } from '../store';
import type { PaneId } from '../store';

export interface Command {
  id: string;
  title: string;
  subtitle?: string;
  run: () => void;
  enabled?: () => boolean;
}

const paneCmd = (pane: PaneId, n: number): Command => ({
  id: `pane.${pane}`,
  title: `Pane: ${pane[0].toUpperCase()}${pane.slice(1)}`,
  subtitle: `Switch focus to the ${pane} pane (${n})`,
  run: () => useStore.getState().setPane(pane),
  enabled: () => !!useStore.getState().focusedId,
});

export function buildCommands(): Command[] {
  const s = () => useStore.getState();
  return [
    { id: 'agent.spawn', title: 'Spawn agent…', subtitle: 'Open the dir-first quick-spawn palette', run: () => s().setModal('spawn') },
    { id: 'agent.resume', title: 'Resume session…', subtitle: 'Find a Tandem or external ACP session', run: () => s().setModal('resume') },
    {
      id: 'agent.spawn.sibling',
      title: 'Spawn sibling agent',
      subtitle: 'New agent in the focused repo (new worktree)',
      enabled: () => !!s().focusedId,
      run: () => {
        const st = s();
        const a = st.focusedId ? st.agents[st.focusedId] : undefined;
        if (a && a.workspace.kind === 'worktree' && a.workspace.repoPath) {
          void st.spawn({ adapter: 'acp', workspace: { kind: 'worktree', repo: a.workspace.repoPath } });
          return;
        } else {
          st.setModal('spawn');
        }
      },
    },
    { id: 'palette.open', title: 'Command palette', subtitle: 'All commands, jump-to-agent', run: () => s().setModal('command') },
    { id: 'nav.goToAgent', title: 'Go to agent…', subtitle: 'Jump to an agent by name', run: () => s().setModal('command') },
    { id: 'nav.next', title: 'Next agent', subtitle: 'Move down the agent rail', run: () => s().nav(1) },
    { id: 'nav.prev', title: 'Previous agent', subtitle: 'Move up the agent rail', run: () => s().nav(-1) },
    paneCmd('transcript', 1),
    paneCmd('terminal', 2),
    paneCmd('diff', 3),
    paneCmd('browser', 4),
    {
      id: 'approvals.approveFocused',
      title: 'Approve top request',
      subtitle: 'Allow the most-urgent pending approval',
      enabled: () => allApprovals(s()).length > 0,
      run: () => {
        const top = allApprovals(s())[0];
        if (top) {
          const allow = top.approval.options.find((o) => /allow|yes|approve/i.test(o.name)) ?? top.approval.options[0];
          s().respond(top.agentId, top.approval.reqId, allow.optionId);
          s().focus(top.agentId);
        }
      },
    },
    {
      id: 'approvals.denyFocused',
      title: 'Deny top request',
      subtitle: 'Reject the most-urgent pending approval',
      enabled: () => allApprovals(s()).length > 0,
      run: () => {
        const top = allApprovals(s())[0];
        if (top) {
          const deny = top.approval.options.find((o) => /reject|deny|no/i.test(o.name)) ?? top.approval.options[top.approval.options.length - 1];
          s().respond(top.agentId, top.approval.reqId, deny.optionId);
          s().focus(top.agentId);
        }
      },
    },
    {
      id: 'agent.interrupt',
      title: 'Interrupt agent',
      subtitle: 'Cancel the focused agent’s current turn',
      enabled: () => {
        const st = s();
        return !!st.focusedId && st.agents[st.focusedId]?.status === 'working';
      },
      run: () => {
        const st = s();
        if (st.focusedId) st.interrupt(st.focusedId);
      },
    },
    {
      id: 'agent.close',
      title: 'Close agent',
      subtitle: 'Tear down the focused agent (keeps its branch)',
      enabled: () => !!s().focusedId,
      run: () => {
        const st = s();
        if (!st.focusedId) return;
        void st.closeAgent(st.focusedId).then((r) => {
          if (r.error?.startsWith('dirty_worktree')) {
            if (confirm(`${st.focusedId} has uncommitted changes. Force close and drop the checkout? (branch is kept)`)) {
              void st.closeAgent(st.focusedId!, true);
            }
          }
        });
      },
    },
    {
      id: 'browser.toggleWheel',
      title: 'Take / release the wheel',
      subtitle: 'Grab or hand back control of the focused agent’s shared browser',
      enabled: () => {
        const st = s();
        return !!st.focusedId && !!st.agents[st.focusedId]?.browserActive;
      },
      run: () => {
        const st = s();
        if (st.focusedId) {
          st.setPane('browser');
          st.toggleWheel(st.focusedId);
        }
      },
    },
    { id: 'theme.toggle', title: 'Toggle theme', subtitle: 'Switch light / dark', run: () => s().toggleTheme() },
    { id: 'inspector.toggle', title: 'Toggle inspector', subtitle: 'Show / hide the bottom inspector', run: () => s().toggleInspector() },
  ];
}

// Convenience lookup used by the global key handler.
export function runCommand(id: string): void {
  const cmd = buildCommands().find((c) => c.id === id);
  if (cmd && (!cmd.enabled || cmd.enabled())) cmd.run();
}
