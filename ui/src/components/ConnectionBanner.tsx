import { useStore } from '../store';

// Round trips beyond this make the UI feel stuck even though the socket is up.
const SLOW_LINK_RTT_MS = 1500;

// Subtle banner while the socket is (re)connecting, or while it is open but the
// daemon is answering slowly or not at all. Silent when healthy.
export function ConnectionBanner() {
  const conn = useStore((s) => s.conn);
  const link = useStore((s) => s.linkHealth);
  if (conn === 'reconnecting') return <div className="conn-banner reconnecting">Reconnecting to daemon…</div>;
  if (conn === 'connecting') return <div className="conn-banner connecting">Connecting…</div>;
  if (conn !== 'connected') return null;
  if (link.stale) {
    return <div className="conn-banner reconnecting">Daemon is not responding — actions may be delayed until the link recovers.</div>;
  }
  if (link.rttMs !== null && link.rttMs >= SLOW_LINK_RTT_MS) {
    return <div className="conn-banner reconnecting">Slow link to daemon ({(link.rttMs / 1000).toFixed(1)} s round trip) — actions may lag.</div>;
  }
  return null;
}
