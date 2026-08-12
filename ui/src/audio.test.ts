import { describe, expect, it } from 'vitest';
import { lastAgentMessage, speechText } from './audio';

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
