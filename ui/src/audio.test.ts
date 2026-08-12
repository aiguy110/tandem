import { afterEach, describe, expect, it, vi } from 'vitest';
import { lastAgentMessage, lastAgentReply, renderMessageAudio } from './audio';

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

  it('keeps the first message chunk sequence for the audio endpoint', () => {
    expect(lastAgentReply([
      { seq: 3, event: { kind: 'message_chunk', text: 'Final ' } },
      { seq: 4, event: { kind: 'message_chunk', text: 'reply.' } },
      { seq: 5, event: { kind: 'status', status: 'idle' } },
    ])).toEqual({ seq: 3, text: 'Final reply.' });
  });
});

describe('renderMessageAudio', () => {
  it('uses the same authenticated audio endpoint as Listen', async () => {
    localStorage.setItem('tandem.token', 'test-token');
    const fetchMock = vi.fn().mockResolvedValue(new Response(new Blob(['audio'], { type: 'audio/mpeg' }), { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    (URL as typeof URL & { createObjectURL: (blob: Blob) => string }).createObjectURL = vi.fn().mockReturnValue('blob:voice');

    await expect(renderMessageAudio('agent one', 42)).resolves.toBe('blob:voice');
    expect(fetchMock).toHaveBeenCalledWith('/api/agents/agent%20one/messages/42/audio', {
      method: 'POST', headers: { Authorization: 'Bearer test-token' },
    });
  });
});
