import { describe, expect, it } from 'vitest';
import { addressRef, agentRef } from './messaging';
import type { FederationHost } from './wire';

const hosts: FederationHost[] = [
  { id: 'local', local: true, name: 'llu', status: 'connected', nodeId: 'node-a' },
  { id: 'b', name: 'bifrost', status: 'connected', nodeId: 'node-b' },
  { id: 'c', name: 'build box', status: 'connected', nodeId: 'node-c' },
];

describe('agentRef', () => {
  it('uses the host and agent display names when they are plain tokens', () => {
    expect(agentRef({ id: 's1', name: 'gazepoint' }, hosts)).toBe('@agent:llu/gazepoint');
    expect(agentRef({ id: 'fed~b~s2', name: 'slow-drag', hostId: 'b' }, hosts)).toBe('@agent:bifrost/slow-drag');
  });

  it('falls back to the host-local session ID when the name is not a plain token', () => {
    expect(agentRef({ id: 's1', name: 'Mobile test' }, hosts)).toBe('@agent:llu/s1');
    expect(agentRef({ id: 'fed~b~sess-9', name: '', hostId: 'b' }, hosts)).toBe('@agent:bifrost/sess-9');
    expect(agentRef({ id: 's1', name: '-leading' }, hosts)).toBe('@agent:llu/s1');
  });

  it('falls back to the node ID when the host name is not a plain token', () => {
    expect(agentRef({ id: 'fed~c~s3', name: 'worker', hostId: 'c' }, hosts)).toBe('@agent:node-c/worker');
  });

  it('falls back to the node ID for an unnamed host', () => {
    expect(agentRef({ id: 's1', name: 'x' }, [{ id: 'local', local: true, nodeId: 'node-a' }])).toBe('@agent:node-a/x');
  });
});

describe('addressRef', () => {
  it('applies the same rule to an address outside the store', () => {
    expect(addressRef(hosts, { host: 'node-b', agent: 'sess-b', name: 'slow-drag' })).toBe('@agent:bifrost/slow-drag');
    expect(addressRef(hosts, { host: 'node-c', agent: 'sess-c', name: 'two words' })).toBe('@agent:node-c/sess-c');
    expect(addressRef(hosts, { host: 'node-z', agent: 'sess-z' })).toBe('@agent:node-z/sess-z');
  });
});
