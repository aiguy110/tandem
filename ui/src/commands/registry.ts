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

const paneCmd = (pane: PaneId, n: number, label: string): Command => ({
  id: `pane.${pane}`,
  title: `Pane: ${label}`,
  subtitle: `Switch focus to the ${label} pane (${n})`,
  run: () => useStore.getState().setPane(pane),
  enabled: () => !!useStore.getState().focusedId,
});

export function buildCommands(): Command[] {
  const s = () => useStore.getState();
  return [
    { id: 'agent.spawn', title: 'Spawn session…', subtitle: 'Open the dir-first quick-spawn palette', run: () => s().setModal('spawn') },
    { id: 'agent.resume', title: 'Resume session…', subtitle: 'Search past and active sessions, grouped by repo', run: () => s().setModal('resume') },
    { id: 'automation.open', title: 'Automation…', subtitle: 'View schedules and recent runs', run: () => s().setModal('automation') },
    {
      id: 'agent.spawn.sibling',
      title: 'Spawn sibling session',
      subtitle: 'New session in the focused repo (new worktree)',
      enabled: () => !!s().focusedId,
      run: () => {
        const st = s();
        const a = st.focusedId ? st.sessions[st.focusedId] : undefined;
        if (a && a.workspace.kind === 'worktree' && a.workspace.repoPath) {
          const targetRef = a.workspace.targetRef ?? 'HEAD';
          void st.spawn({
            adapter: 'acp',
            agent: a.agent,
            workspace: {
              kind: 'worktree',
              repo: a.workspace.repoPath,
              branchMode: 'create',
              source: { ref: targetRef },
              integration: a.workspace.targetRef ? { kind: a.workspace.targetKind ?? 'detached', ref: a.workspace.targetRef } : undefined,
            },
          });
          return;
        } else {
          st.setModal('spawn');
        }
      },
    },
    {
      id: 'agent.spawn.sibling.dependent',
      title: 'Spawn dependent sibling session',
      subtitle: 'New session starting from the focused session’s current commits',
      enabled: () => {
        const st = s();
        const a = st.focusedId ? st.sessions[st.focusedId] : undefined;
        return !!a && a.workspace.kind === 'worktree' && !!a.workspace.repoPath && !!a.workspace.branch;
      },
      run: () => {
        const st = s();
        const a = st.focusedId ? st.sessions[st.focusedId] : undefined;
        if (!a || a.workspace.kind !== 'worktree' || !a.workspace.repoPath || !a.workspace.branch) return;
        void st.spawn({
          adapter: 'acp',
          agent: a.agent,
          workspace: {
            kind: 'worktree',
            repo: a.workspace.repoPath,
            branchMode: 'create',
            source: { ref: `refs/heads/${a.workspace.branch}` },
            integration: a.workspace.targetRef ? { kind: a.workspace.targetKind ?? 'detached', ref: a.workspace.targetRef } : undefined,
          },
        });
      },
    },
    { id: 'palette.open', title: 'Command palette', subtitle: 'All commands, jump-to-session', run: () => s().setModal('command') },
    { id: 'nav.goToAgent', title: 'Go to session…', subtitle: 'Jump to a session by name', run: () => s().setModal('command') },
    { id: 'nav.next', title: 'Next session', subtitle: 'Move down the session rail', run: () => s().nav(1) },
    { id: 'nav.prev', title: 'Previous session', subtitle: 'Move up the session rail', run: () => s().nav(-1) },
    paneCmd('chat', 1, 'Chat'),
    paneCmd('shell', 2, 'Terminal'),
    paneCmd('diff', 3, 'Diff'),
    paneCmd('browser', 4, 'Browser'),
    {
      id: 'approvals.approveFocused',
      title: 'Approve top request',
      subtitle: 'Allow the most-urgent pending approval',
      enabled: () => allApprovals(s()).length > 0,
      run: () => {
        const top = allApprovals(s())[0];
        if (top) {
          const allow = top.approval.options.find((o) => /allow|yes|approve/i.test(o.name)) ?? top.approval.options[0];
          s().respond(top.sessionId, top.approval.reqId, allow.optionId);
          s().focus(top.sessionId);
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
          s().respond(top.sessionId, top.approval.reqId, deny.optionId);
          s().focus(top.sessionId);
        }
      },
    },
    {
      id: 'agent.interrupt',
      title: 'Interrupt session',
      subtitle: 'Cancel the focused session’s current turn',
      enabled: () => {
        const st = s();
        return !!st.focusedId && st.sessions[st.focusedId]?.status === 'working';
      },
      run: () => {
        const st = s();
        if (st.focusedId) st.interrupt(st.focusedId);
      },
    },
    {
      id: 'agent.close',
      title: 'Close session',
      subtitle: 'Tear down the focused session (keeps its branch)',
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
      subtitle: 'Grab or hand back control of the focused session’s shared browser',
      enabled: () => {
        const st = s();
        return !!st.focusedId && !!st.sessions[st.focusedId]?.browserActive;
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
