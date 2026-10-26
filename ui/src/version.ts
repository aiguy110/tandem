// This is a build compatibility marker, not a displayed version. The latter
// is deliberately fetched from the daemon so it cannot go stale in the UI.
export const frontendVersion = (import.meta.env.VITE_TANDEM_VERSION as string | undefined) ?? 'dev';

export interface DaemonVersion {
  version: string;
}

export async function loadDaemonVersion(signal?: AbortSignal): Promise<DaemonVersion> {
  const response = await fetch('/version', { cache: 'no-store', signal });
  if (!response.ok) throw new Error(`load daemon version: ${response.status}`);
  const value: unknown = await response.json();
  if (!value || typeof value !== 'object' || typeof (value as DaemonVersion).version !== 'string') {
    throw new Error('load daemon version: invalid response');
  }
  return value as DaemonVersion;
}

const reloadAttemptKey = 'tandem.versionReloadAttempt';

// Reloads once per daemon version to pick up a freshly deployed UI. Returns
// false when this tab already reloaded for that version and still runs a
// different bundle: the daemon is serving a stale embedded UI, and reloading
// again would only loop.
export function reloadForDaemonVersion(daemonVersion: string): boolean {
  if (daemonVersion === frontendVersion) {
    sessionStorage.removeItem(reloadAttemptKey);
    return false;
  }
  if (sessionStorage.getItem(reloadAttemptKey) === daemonVersion) return false;
  sessionStorage.setItem(reloadAttemptKey, daemonVersion);
  window.location.reload();
  return true;
}
