import { describe, expect, it, vi } from 'vitest';
import { shellHub } from './shellHub';
import { ptyHub } from './ptyHub';

for (const [name, hub] of [['shell', shellHub], ['pty', ptyHub]] as const) {
  describe(`${name} replay provenance`, () => {
    it('marks buffered and snapshot output as replay while retaining live output', () => {
      const id = 'replay-test';
      hub.push(id, btoa('history'));
      const listener = vi.fn();
      const unsubscribe = hub.subscribe(id, listener);
      expect(listener.mock.calls[0][1]).toBe(true);
      hub.push(id, btoa('snapshot'), true);
      expect(listener.mock.calls[1][1]).toBe(true);
      hub.push(id, btoa('live'));
      expect(listener.mock.calls[2][1]).toBe(false);
      unsubscribe();
      hub.clear(id);
    });
  });
}
