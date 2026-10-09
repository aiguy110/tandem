import { useEffect, useMemo, useState } from 'react';
import { useStore } from '../store';
import type { HostMetric } from '../wire';

const pct = (used: number, total: number) => total > 0 ? Math.max(0, Math.min(100, used / total * 100)) : 0;
const bytes = (value: number) => `${(value / 1024 / 1024 / 1024).toFixed(value >= 100 * 1024 ** 3 ? 0 : 1)} GiB`;

function Sparkline({ values, color }: { values: number[]; color: string }) {
  const points = values.map((value, i) => `${values.length < 2 ? 0 : i / (values.length - 1) * 100},${40 - value * .4}`).join(' ');
  return <svg className="metric-chart" viewBox="0 0 100 40" preserveAspectRatio="none" aria-hidden="true"><polyline points={points} fill="none" stroke={color} strokeWidth="1.4" vectorEffect="non-scaling-stroke" /></svg>;
}

export function HostMetricsModal() {
  const hostId = useStore((s) => s.hostMetricsHostId) ?? 'local';
  const host = useStore((s) => s.hosts.find((item) => item.id === hostId));
  const fetchMetrics = useStore((s) => s.fetchHostMetrics);
  const setModal = useStore((s) => s.setModal);
  const [metrics, setMetrics] = useState<HostMetric[]>([]);
  const [error, setError] = useState('');
  useEffect(() => {
    let active = true;
    const load = () => void fetchMetrics(hostId).then((next) => { if (active) { setMetrics(next); setError(''); } }).catch((e: unknown) => { if (active) setError(e instanceof Error ? e.message : String(e)); });
    load(); const timer = window.setInterval(load, 5000);
    return () => { active = false; window.clearInterval(timer); };
  }, [fetchMetrics, hostId]);
  const latest = metrics.at(-1);
  const rows = useMemo(() => latest ? [
    { name: 'CPU', value: latest.cpuPercent, detail: `${latest.cpuPercent.toFixed(1)}%`, series: metrics.map((m) => m.cpuPercent), color: '#60a5fa' },
    { name: 'RAM', value: pct(latest.memoryUsed, latest.memoryTotal), detail: `${bytes(latest.memoryUsed)} / ${bytes(latest.memoryTotal)}`, series: metrics.map((m) => pct(m.memoryUsed, m.memoryTotal)), color: '#34d399' },
    { name: 'DISK', value: pct(latest.diskUsed, latest.diskTotal), detail: `${bytes(latest.diskUsed)} / ${bytes(latest.diskTotal)}`, series: metrics.map((m) => pct(m.diskUsed, m.diskTotal)), color: '#fbbf24' },
    ...(latest.vramTotal ? [{ name: 'vRAM', value: pct(latest.vramUsed ?? 0, latest.vramTotal), detail: `${bytes(latest.vramUsed ?? 0)} / ${bytes(latest.vramTotal)}`, series: metrics.map((m) => pct(m.vramUsed ?? 0, m.vramTotal ?? 0)), color: '#c084fc' }] : []),
  ] : [], [latest, metrics]);
  return <div className="modal-scrim" onMouseDown={(e) => e.target === e.currentTarget && setModal('none')}>
    <div className="modal host-metrics-modal" role="dialog" aria-modal="true" aria-labelledby="host-metrics-title" onKeyDown={(event) => event.key === 'Escape' && setModal('none')}>
      <div className="fleet-header"><div><div className="primary" id="host-metrics-title">{host?.name ?? hostId} utilization</div><div className="sub">Live and historical host resources · last 24 hours</div></div><button type="button" className="fleet-close" onClick={() => setModal('none')} aria-label="Close host utilization">×</button></div>
      {error && <div className="modal-err">{error}</div>}
      <div className="host-metrics-grid">{rows.map((row) => <section key={row.name}><div className="metric-heading"><strong>{row.name}</strong><span>{row.value.toFixed(1)}%</span></div><div className="metric-detail">{row.detail}</div><Sparkline values={row.series} color={row.color} /></section>)}</div>
      {!error && !latest && <div className="host-metrics-empty">Waiting for the first sample…</div>}
      <div className="foot host-metrics-foot"><span>{metrics.length} samples{latest ? ` · updated ${new Date(latest.ts).toLocaleTimeString()}` : ''}</span><button type="button" onClick={() => setModal('none')}>Close</button></div>
    </div>
  </div>;
}
