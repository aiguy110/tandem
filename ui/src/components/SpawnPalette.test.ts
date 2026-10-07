import { describe, expect, it } from 'vitest';
import type { WireEvent } from '../wire';
import { delegatedWorkPreview } from './SpawnPalette';

function events(...event: WireEvent[]): { seq: number; event: WireEvent }[] {
  return event.map((item, index) => ({ seq: index + 1, event: item }));
}

describe('delegatedWorkPreview', () => {
  it('summarizes top-level delegated roots using their latest statuses', () => {
    const preview = delegatedWorkPreview(events(
      { kind: 'tool_call', id: 'done-task', title: 'Explore auth', status: 'running' },
      { kind: 'message_chunk', text: 'finding', parentId: 'done-task' },
      { kind: 'tool_call_update', id: 'done-task', status: 'done' },
      { kind: 'tool_call', id: 'active-task', title: 'Write tests', status: 'pending' },
      { kind: 'thought_chunk', text: 'working', parentId: 'active-task' },
      { kind: 'tool_call', id: 'failed-task', title: 'Check deploy', status: 'error' },
      { kind: 'tool_call', id: 'child-tool', title: 'Read logs', status: 'done', parentId: 'failed-task' },
    ));

    expect(preview).toEqual({ completed: 1, failed: 1, active: 1, activeTitles: ['Write tests'] });
  });

  it('does not double-count nested delegates', () => {
    const preview = delegatedWorkPreview(events(
      { kind: 'tool_call', id: 'root', title: 'Root delegate', status: 'running' },
      { kind: 'tool_call', id: 'nested', title: 'Nested delegate', status: 'done', parentId: 'root' },
      { kind: 'message_chunk', text: 'nested answer', parentId: 'nested' },
    ));

    expect(preview).toEqual({ completed: 0, failed: 0, active: 1, activeTitles: ['Root delegate'] });
  });

  it('is absent for older flat transcripts', () => {
    expect(delegatedWorkPreview(events(
      { kind: 'tool_call', id: 'task', title: 'Task', status: 'done' },
      { kind: 'message_chunk', text: 'ordinary output' },
    ))).toBeNull();
  });
});
