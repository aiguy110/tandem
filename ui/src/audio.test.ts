import { afterEach, describe, expect, it, vi } from 'vitest';
import { lastAgentMessage, speak, speechText } from './audio';

afterEach(() => vi.unstubAllGlobals());

describe('lastAgentMessage', () => {
  it('returns only the final contiguous assistant message', () => {
    expect(lastAgentMessage([
      { seq: 1, event: { kind: 'message_chunk', text: 'Earlier reply.' } },
      { seq: 2, event: { kind: 'tool_call', id: 'tool-1', title: 'Run check', status: 'done' } },
      { seq: 3, event: { kind: 'message_chunk', text: 'Final ' } },
      { seq: 4, event: { kind: 'message_chunk', text: 'reply.' } },
      { seq: 5, event: { kind: 'status', status: 'idle' } },
    ])).toBe('Final reply.');
  });
});

describe('speechText', () => {
  it('keeps readable content while removing common markdown decoration', () => {
    expect(speechText('See [the docs](https://example.com), then `run it`.')).toBe('See [the docs], then run it.');
  });
});

describe('speak', () => {
  it('reports the browser speech lifecycle', () => {
    class FakeUtterance {
      onstart: (() => void) | null = null;
      onend: (() => void) | null = null;
      onerror: ((event: { error: string }) => void) | null = null;
      constructor(_text: string) {}
    }
    vi.stubGlobal('SpeechSynthesisUtterance', FakeUtterance);
    vi.stubGlobal('window', {
      speechSynthesis: {
        cancel: vi.fn(),
        speak: (utterance: FakeUtterance) => {
          utterance.onstart?.();
          utterance.onend?.();
        },
      },
    });
    const events: string[] = [];

    speak('Hello!', { onStart: () => events.push('start'), onEnd: () => events.push('end') });

    expect(events).toEqual(['start', 'end']);
  });

  it('reports unavailable speech instead of failing silently', () => {
    vi.stubGlobal('window', {});
    const unavailable = vi.fn();

    speak('Hello!', { onUnavailable: unavailable });

    expect(unavailable).toHaveBeenCalledOnce();
  });
});
