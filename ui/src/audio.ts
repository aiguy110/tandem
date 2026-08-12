import type { WireEvent } from './wire';

// Browser speech is deliberately a UI-only preference: it is tied to the
// browser/device playing the audio and must never be triggered by transcript
// replay in another client.
export function lastAgentMessage(events: { seq: number; event: WireEvent }[]): string | null {
  let end = -1;
  for (let i = events.length - 1; i >= 0; i--) {
    if (events[i].event.kind === 'message_chunk') {
      end = i;
      break;
    }
  }
  if (end === -1) return null;

  const chunks: string[] = [];
  for (let i = end; i >= 0; i--) {
    const event = events[i].event;
    if (event.kind !== 'message_chunk') break;
    chunks.unshift(event.text);
  }
  const message = chunks.join('').trim();
  return message || null;
}

// Keep the spoken result natural without changing the response's meaning.
// Code itself is retained; only common Markdown delimiters are removed.
export function speechText(message: string): string {
  return message
    .replace(/!?(\[[^\]]*\])\([^\s)]+(?:\s+[^)]*)?\)/g, '$1')
    .replace(/[`*_~>#]/g, '')
    .replace(/\s+/g, ' ')
    .trim();
}

export function speak(message: string): void {
  if (typeof window === 'undefined' || !('speechSynthesis' in window) || typeof SpeechSynthesisUtterance === 'undefined') return;
  const text = speechText(message);
  if (!text) return;
  // Avoid a backlog when multiple agents finish close together.
  window.speechSynthesis.cancel();
  window.speechSynthesis.speak(new SpeechSynthesisUtterance(text));
}
