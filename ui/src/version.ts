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
