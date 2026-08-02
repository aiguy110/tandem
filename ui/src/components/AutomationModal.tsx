import { useMemo, useState } from 'react';
import { useStore } from '../store';
import type { AutomationRun } from '../wire';

const formatTime = (timestamp?: number): string => {
  if (!timestamp) return 'Never run';
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(new Date(timestamp));
};

const lastTimestamp = (run: AutomationRun): number => run.completedAt ?? run.startedAt;

export function AutomationModal() {
  const setModal = useStore((s) => s.setModal);
  const jobs = useStore((s) => s.automationJobs);
  const runs = useStore((s) => s.automationRuns);
  const loading = useStore((s) => s.automationLoading);
  const error = useStore((s) => s.automationError);
  const refresh = useStore((s) => s.refreshAutomation);
  const setEnabled = useStore((s) => s.setAutomationEnabled);
  const [changing, setChanging] = useState<string | null>(null);

  const latestRuns = useMemo(() => {
    const latest = new Map<string, AutomationRun>();
    for (const run of runs) {
      if (!run.jobId) continue;
      const current = latest.get(run.jobId);
      if (!current || lastTimestamp(run) > lastTimestamp(current)) latest.set(run.jobId, run);
    }
    return latest;
  }, [runs]);

  const toggle = async (id: string, enabled: boolean) => {
    setChanging(id);
    try {
      await setEnabled(id, enabled);
    } catch {
      // The correlated response is rendered through automationError.
    } finally {
      setChanging(null);
    }
  };

  return (
    <div className="modal-scrim" onMouseDown={(event) => event.target === event.currentTarget && setModal('none')}>
      <div className="modal automation-modal" role="dialog" aria-modal="true" aria-labelledby="automation-title" onKeyDown={(event) => event.key === 'Escape' && setModal('none')}>
        <div className="automation-header">
          <div>
            <div className="primary" id="automation-title">Automation</div>
            <div className="sub">Registered repository schedules and their latest runs</div>
          </div>
          <button type="button" className="automation-close" onClick={() => setModal('none')} aria-label="Close automation">×</button>
        </div>
        <div className="rows automation-rows">
          {loading && jobs.length === 0 && <div className="empty">Loading automation…</div>}
          {!loading && !error && jobs.length === 0 && <div className="empty">No scheduled scripts are registered.</div>}
          {jobs.map((job) => {
            const run = latestRuns.get(job.id);
            return (
              <div className="automation-row" key={job.id}>
                <div className="automation-main">
                  <div className="automation-title-line">
                    <span className="primary">{job.name || job.scriptPath.split('/').pop()}</span>
                    <span className={`automation-state ${job.enabled ? 'enabled' : 'paused'}`}>{job.enabled ? 'enabled' : 'paused'}</span>
                  </div>
                  <div className="automation-path" title={job.scriptPath}>{job.scriptPath}</div>
                  <div className="automation-details">
                    <span>{job.cron}</span>
                    {job.timezone && <span>{job.timezone}</span>}
                    <span>{run ? formatTime(lastTimestamp(run)) : 'Never run'}</span>
                    {run && <span className={`automation-outcome outcome-${run.outcome}`}>{run.outcome.replaceAll('_', ' ')}</span>}
                  </div>
                </div>
                <button
                  type="button"
                  className="automation-toggle"
                  disabled={changing === job.id}
                  onClick={() => void toggle(job.id, !job.enabled)}
                >
                  {changing === job.id ? '…' : job.enabled ? 'Pause' : 'Resume'}
                </button>
              </div>
            );
          })}
        </div>
        {error && <div className="modal-err">{error}</div>}
        <div className="foot automation-foot">
          <span>{jobs.length} {jobs.length === 1 ? 'job' : 'jobs'}</span>
          <button type="button" disabled={loading} onClick={() => void refresh().catch(() => undefined)}>Refresh</button>
        </div>
      </div>
    </div>
  );
}
