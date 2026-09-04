// Global Vitest setup. jsdom doesn't implement URL.createObjectURL /
// URL.revokeObjectURL, which the audio engine's silence keepalive
// (ui/src/audio/engine.ts) calls unconditionally in production (it's on by
// default — see EngineState.keepaliveEnabled). Stub them here once so every
// test file gets a working fake blob URL instead of each suite needing its
// own workaround, and real browsers' behavior isn't shadowed for suites that
// don't care about this at all.
import { vi } from 'vitest';

if (typeof URL.createObjectURL !== 'function') {
  let counter = 0;
  URL.createObjectURL = vi.fn(() => `blob:test-${counter++}`) as typeof URL.createObjectURL;
}
if (typeof URL.revokeObjectURL !== 'function') {
  URL.revokeObjectURL = vi.fn() as typeof URL.revokeObjectURL;
}
