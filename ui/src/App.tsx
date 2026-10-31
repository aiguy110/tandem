import { useEffect, useState, type CSSProperties } from 'react';
import { useStore } from './store';
import { useGlobalKeys } from './useGlobalKeys';
import { TokenScreen } from './components/TokenScreen';
import { ConnectionBanner } from './components/ConnectionBanner';
import { ConductorBar } from './components/ConductorBar';
import { SessionsRail } from './components/AgentsRail';
import { ApprovalsRail } from './components/ApprovalsRail';
import { FocusArea } from './components/FocusArea';
import { Inspector } from './components/Inspector';
import { SpawnPalette } from './components/SpawnPalette';
import { CommandPalette } from './components/CommandPalette';
import { AdapterManagerModal } from './components/AdapterManagerModal';
import { McpServersModal } from './components/McpServersModal';
import { ResumePalette } from './components/ResumePalette';
import { AutomationModal } from './components/AutomationModal';
import { FleetView } from './components/FleetView';
import { HostMetricsModal } from './components/HostMetricsModal';
import { AudioEngineRoot } from './components/audio/AudioEngineRoot';
import { AppearanceModal } from './components/AppearanceModal';
import { uiScale } from './appearance';
import { useValuePresence } from './transitions';
import { ArtifactPage } from './components/ArtifactView';
import { parseArtifactRoute, useLocationHash } from './artifacts';
import { frontendVersion, loadDaemonVersion, reloadForDaemonVersion } from './version';

export function App() {
  const theme = useStore((s) => s.theme);
  const appearance = useStore((s) => s.appearance);
  const conn = useStore((s) => s.conn);
  const modal = useStore((s) => s.modal);
  const boot = useStore((s) => s.boot);
	const sessionsRailCollapsed = useStore((s) => s.sessionsRailCollapsed);
  const approvalsRailCollapsed = useStore((s) => s.approvalsRailCollapsed);
  // Deliberately component-local: dock widths reset with each page load.
  const defaultDockWidths = () => window.innerWidth <= 860
		? { sessions: 200, approvals: 260 }
		: { sessions: 240, approvals: 300 };
  const [dockWidths, setDockWidths] = useState(defaultDockWidths);

	const resizeDock = (dock: 'sessions' | 'approvals', startX: number) => {
    const startWidth = dockWidths[dock];
		const otherWidth = dockWidths[dock === 'sessions' ? 'approvals' : 'sessions'];
		const direction = dock === 'sessions' ? 1 : -1;
    const onMove = (event: PointerEvent) => {
      const maxWidth = Math.max(160, window.innerWidth - otherWidth - 240);
      const width = Math.max(160, Math.min(maxWidth, startWidth + direction * (event.clientX - startX)));
      setDockWidths((current) => ({ ...current, [dock]: width }));
    };
    const onEnd = () => {
      document.body.classList.remove('dock-resizing');
      window.removeEventListener('pointermove', onMove);
      window.removeEventListener('pointerup', onEnd);
    };
    document.body.classList.add('dock-resizing');
    window.addEventListener('pointermove', onMove);
    window.addEventListener('pointerup', onEnd);
  };

  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme);
  }, [theme]);

  // Every --fs-* token is expressed as a multiple of --ui-scale (styles.css),
  // so the whole UI resizes from this one property.
  useEffect(() => {
    document.documentElement.style.setProperty('--ui-scale', String(uiScale(appearance)));
  }, [appearance]);

  useEffect(() => {
    boot();
  }, [boot]);

  const [daemonVersion, setDaemonVersion] = useState<string | null>(null);

  useEffect(() => {
    // Check only once the socket is connected. In particular, this happens
    // after the reconnect banner clears when the daemon has rebuilt itself.
    if (conn !== 'connected') return;
    const controller = new AbortController();
    void loadDaemonVersion(controller.signal).then(({ version }) => {
      if (reloadForDaemonVersion(version)) return;
      setDaemonVersion(version);
    }).catch((error: unknown) => {
      if (!(error instanceof DOMException && error.name === 'AbortError')) {
        // A transient failure should not take mission control offline. Keep the
        // loading placeholder, and let the next browser refresh retry.
        console.warn('Could not load Tandem version', error);
      }
    });
    return () => controller.abort();
  }, [conn]);

  useGlobalKeys();
  const artifactRoute = parseArtifactRoute(useLocationHash());

  if (conn === 'need-token' || conn === 'rejected') {
    return <TokenScreen rejected={conn === 'rejected'} />;
  }

  return (
    <>
      <AudioEngineRoot />
      <div
		className={`app${sessionsRailCollapsed ? ' sessions-collapsed' : ''}${approvalsRailCollapsed ? ' approvals-collapsed' : ''}`}
		style={{ '--sessions-rail-width': `${dockWidths.sessions}px`, '--approvals-rail-width': `${dockWidths.approvals}px` } as CSSProperties}
      >

        <ConductorBar version={daemonVersion} />
		<SessionsRail onResizeStart={(x) => resizeDock('sessions', x)} />
        <FocusArea />
        <ApprovalsRail onResizeStart={(x) => resizeDock('approvals', x)} />
        <Inspector />
      </div>
      <ConnectionBanner />
      {daemonVersion !== null && daemonVersion !== frontendVersion && (
        <div className="conn-banner mismatch">
          UI {frontendVersion} does not match daemon {daemonVersion}; the daemon is serving a stale UI build.
        </div>
      )}
      <ModalHost modal={modal} />
      {artifactRoute && <ArtifactPage key={`${artifactRoute.sessionId}/${artifactRoute.artifactId}`} route={artifactRoute} />}
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
      {rendered === 'fleet' && <FleetView />}
      {rendered === 'host-metrics' && <HostMetricsModal />}
      {rendered === 'adapters' && <AdapterManagerModal />}
      {rendered === 'mcp' && <McpServersModal />}
      {rendered === 'appearance' && <AppearanceModal />}
    </div>
  );
}
