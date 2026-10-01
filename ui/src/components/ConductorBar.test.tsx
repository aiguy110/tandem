import { act, cleanup, render } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { LOCAL_HOST_ID, useStore } from '../store';
import { ConductorBar } from './ConductorBar';

afterEach(() => {
  cleanup();
  useStore.setState({ messagingPausedByHost: {}, hosts: [{ id: LOCAL_HOST_ID, name: 'This host', status: 'connected', local: true }] });
});

describe('ConductorBar messaging indicator', () => {
  it('appears only while some host has agent messaging paused', () => {
    useStore.setState({ hosts: [{ id: LOCAL_HOST_ID, name: 'This host', status: 'connected', local: true }, { id: 'b', name: 'Builder', status: 'connected' }] });
    const view = render(<ConductorBar version="v1" />);
    expect(view.container.querySelector('.bar-messaging-paused')).toBeNull();

    act(() => useStore.setState({ messagingPausedByHost: { b: true } }));
    const badge = view.container.querySelector('.bar-messaging-paused');
    expect(badge?.textContent).toContain('messaging paused');
    expect(badge?.getAttribute('title')).toContain('Builder');

    act(() => useStore.setState({ messagingPausedByHost: { b: false } }));
    expect(view.container.querySelector('.bar-messaging-paused')).toBeNull();
  });
});
