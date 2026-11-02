import { describe, expect, it, vi } from 'vitest';
import { Adapter } from './TerminalRenderer';

function setup() {
  let emit: (data: string) => void = () => {};
  const completions: (() => void)[] = [];
  const term = {
    cols: 80, rows: 24,
    open() {}, resize() {}, focus() {}, dispose: vi.fn(), loadAddon() {},
    onData(cb: (data: string) => void) { emit = cb; },
    write: vi.fn((_bytes: Uint8Array | string, done?: () => void) => {
      if (done) completions.push(done);
    }),
  };
  const adapter = new Adapter(term, { fit() {} });
  const input = vi.fn();
  adapter.onData(input);
  return { adapter, term, input, emit: (s: string) => emit(s), complete: () => completions.shift()!() };
}

describe('terminal replay', () => {
  it('suppresses asynchronous historical replies and preserves live replies and typing', () => {
    const t = setup();
    const query = new TextEncoder().encode('\x1b[6n');
    t.adapter.write(query, true);
    t.adapter.write(query);
    expect(t.term.write).toHaveBeenCalledTimes(1);
    t.emit('\x1b[14;56R');
    expect(t.input).not.toHaveBeenCalled();
    t.emit('typing during replay');
    t.emit('\x1b[A');
    expect(t.input.mock.calls).toEqual([['typing during replay'], ['\x1b[A']]);
    t.complete();
    expect(t.term.write).toHaveBeenCalledTimes(2);
    t.emit('\x1b[1;2R');
    expect(t.input).toHaveBeenLastCalledWith('\x1b[1;2R');
    t.complete();
    t.emit('hello');
    expect(t.input).toHaveBeenLastCalledWith('hello');
  });

  it('batches queued replay chunks and does not resume writes after disposal', () => {
    const t = setup();
    t.adapter.write(new Uint8Array([1]), true);
    t.adapter.write(new Uint8Array([2]), true);
    t.adapter.write(new Uint8Array([3]), true);
    t.complete();
    expect(t.term.write.mock.calls[1][0]).toEqual(new Uint8Array([2, 3]));
    t.adapter.write(new Uint8Array([4]));
    t.adapter.dispose();
    t.emit('typing during replay');
    t.emit('\x1b[A');
    expect(t.input.mock.calls).toEqual([['typing during replay'], ['\x1b[A']]);
    t.complete();
    expect(t.term.write).toHaveBeenCalledTimes(2);
  });
});
