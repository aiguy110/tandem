import { useStore } from '../store';

// Subtle banner while the socket is (re)connecting. Silent when connected.
export function ConnectionBanner() {
  const conn = useStore((s) => s.conn);
  if (conn === 'connected') return null;
  if (conn === 'reconnecting') return <div className="conn-banner reconnecting">Reconnecting to daemon…</div>;
  if (conn === 'connecting') return <div className="conn-banner connecting">Connecting…</div>;
  return null;
}
