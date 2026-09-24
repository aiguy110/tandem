// Last-known spawn-palette data (repos and profiles per host), persisted so a
// fresh page load or daemon restart opens the palette populated while the
// daemon rescans. Everything here is a hint the live daemon replaces.
import type { Profile, RepoInfo } from './wire';

// 'loading': requested, nothing known yet. 'cached': a list from an earlier
// page load, not yet confirmed. 'refreshing': the daemon answered from its
// own cache and is rescanning. 'ready': current. 'error': the last request
// failed (any list shown is the previous one).
export type DirsStatus = 'loading' | 'cached' | 'refreshing' | 'ready' | 'error';

export interface HostProfiles {
  profiles: Profile[];
  recentByProject: Record<string, string[]>;
  // The host's daemon predates the batch listing; the palette falls back to
  // asking per repo.
  legacy?: boolean;
}

const KEY = 'tandem.spawnCache.v1';

interface Persisted {
  dirsByHost?: Record<string, RepoInfo[]>;
  profilesByHost?: Record<string, HostProfiles>;
}

function read(): Persisted {
  try {
    const raw = localStorage.getItem(KEY);
    const parsed = raw ? JSON.parse(raw) as Persisted : {};
    return parsed && typeof parsed === 'object' ? parsed : {};
  } catch {
    return {};
  }
}

export function loadSpawnCache(): { dirsByHost: Record<string, RepoInfo[]>; profilesByHost: Record<string, HostProfiles> } {
  const cached = read();
  const profilesByHost: Record<string, HostProfiles> = {};
  // A legacy marker describes a daemon that may since have been upgraded.
  for (const [hostId, entry] of Object.entries(cached.profilesByHost ?? {})) {
    if (entry && !entry.legacy && Array.isArray(entry.profiles)) profilesByHost[hostId] = entry;
  }
  return { dirsByHost: cached.dirsByHost ?? {}, profilesByHost };
}

export function saveSpawnCache(dirsByHost: Record<string, RepoInfo[]>, profilesByHost: Record<string, HostProfiles>) {
  const profiles: Record<string, HostProfiles> = {};
  for (const [hostId, entry] of Object.entries(profilesByHost)) {
    if (!entry.legacy) profiles[hostId] = entry;
  }
  try {
    localStorage.setItem(KEY, JSON.stringify({ dirsByHost, profilesByHost: profiles }));
  } catch {
    // Storage full or unavailable: the cache is only an optimization.
  }
}
