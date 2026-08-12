import type { WireEvent } from './wire';
import { storedToken } from './ws/client';

// Automatic audio is browser-local: only this client pre-renders it, and
// transcript replay in another client never initiates provider work.
export function lastAgentMessage(events: { seq: number; event: WireEvent }[]): string | null {
  return lastAgentReply(events)?.text ?? null;
}

// The sequence is the first chunk in the contiguous reply, which is also the
// transcript row's sequence and the audio API's message identifier.
export function lastAgentReply(events: { seq: number; event: WireEvent }[]): { seq: number; text: string } | null {
  let end = -1;
  for (let i = events.length - 1; i >= 0; i--) {
    if (events[i].event.kind === 'message_chunk') {
      end = i;
      break;
    }
  }
  if (end === -1) return null;

  const chunks: string[] = [];
  let seq = events[end].seq;
  for (let i = end; i >= 0; i--) {
    const event = events[i].event;
    if (event.kind !== 'message_chunk') break;
    chunks.unshift(event.text);
    seq = events[i].seq;
  }
  const message = chunks.join('').trim();
  return message ? { seq, text: message } : null;
}

// Render the exact same provider-backed clip used by the transcript's Listen
// control. The caller owns the returned object URL and must revoke it when it
// replaces or discards the clip.
export async function renderMessageAudio(agentId: string, seq: number): Promise<string> {
  const token = storedToken();
  const response = await fetch(`/api/agents/${encodeURIComponent(agentId)}/messages/${seq}/audio`, {
    method: 'POST',
    headers: token ? { Authorization: `Bearer ${token}` } : {},
  });
  if (!response.ok) {
    let message = `Voice rendering failed (${response.status})`;
    try {
      const body = await response.json() as { error?: string };
      if (body.error) message = body.error;
    } catch { /* retain the status message for non-JSON provider failures */ }
    throw new Error(message);
  }
  return URL.createObjectURL(await response.blob());
}
