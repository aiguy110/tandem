import { useEffect } from 'react';
import { useStore } from './store';
import { useGlobalKeys } from './useGlobalKeys';
import { TokenScreen } from './components/TokenScreen';
import { ConnectionBanner } from './components/ConnectionBanner';
import { ConductorBar } from './components/ConductorBar';
import { AgentsRail } from './components/AgentsRail';
import { ApprovalsRail } from './components/ApprovalsRail';
import { FocusArea } from './components/FocusArea';
import { Inspector } from './components/Inspector';
import { SpawnPalette } from './components/SpawnPalette';
import { CommandPalette } from './components/CommandPalette';
import { ResumePalette } from './components/ResumePalette';
import { AutomationModal } from './components/AutomationModal';
import { AudioEngineRoot } from './components/audio/AudioEngineRoot';
import { useValuePresence } from './transitions';

export function App() {
  const theme = useStore((s) => s.theme);
  const conn = useStore((s) => s.conn);
  const modal = useStore((s) => s.modal);
  const boot = useStore((s) => s.boot);
  const agentsRailCollapsed = useStore((s) => s.agentsRailCollapsed);
  const approvalsRailCollapsed = useStore((s) => s.approvalsRailCollapsed);

  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme);
  }, [theme]);

  useEffect(() => {
    boot();
  }, [boot]);

  useGlobalKeys();

  if (conn === 'need-token' || conn === 'rejected') {
    return <TokenScreen rejected={conn === 'rejected'} />;
  }

  return (
    <>
      <AudioEngineRoot />
      <div
        className={`app${agentsRailCollapsed ? ' agents-collapsed' : ''}${approvalsRailCollapsed ? ' approvals-collapsed' : ''}`}
      >

        <ConductorBar />
        <AgentsRail />
        <FocusArea />
        <ApprovalsRail />
        <Inspector />
      </div>
      <ConnectionBanner />
      <ModalHost modal={modal} />
    </>
  );
}

// Palettes keep rendering for the length of their exit animation after the
// store has already moved on to 'none' -- see src/transitions.ts.
function ModalHost({ modal }: { modal: string }) {
  const { rendered, closing } = useValuePresence(modal === 'none' ? null : modal);
  if (!rendered) return null;
  return (
    <div style={{ display: 'contents' }} className={closing ? 'popup-closing' : undefined}>
      {rendered === 'spawn' && <SpawnPalette />}
      {rendered === 'command' && <CommandPalette />}
      {rendered === 'resume' && <ResumePalette />}
      {rendered === 'automation' && <AutomationModal />}
    </div>
  );
}
