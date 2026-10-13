import { cleanup, fireEvent, render, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { useStore, type SessionView } from '../../store';
import { BrowserPane } from './BrowserPane';

const initial = useStore.getState();
afterEach(() => {
  cleanup();
  useStore.setState(initial, true);
  vi.restoreAllMocks();
});

it('allows viewing tabs while the agent drives, but requires control to navigate', async () => {
  vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue(null);
  const pages = {
    pages: [{ targetId: 'first', title: 'First', url: 'https://example.test' }, { targetId: 'second', title: 'Second', url: 'about:blank' }],
    selectedTargetId: 'first', canGoBack: true, canGoForward: true,
  };
  const browserPages = vi.fn(async (_sessionId: string, targetId?: string) => ({ ...pages, selectedTargetId: targetId ?? 'first' }));
  const browserNavigate = vi.fn(async () => ({}));
  const session = { id: 'test', browserActive: true, browserOwner: 'agent', takeovers: [] } as unknown as SessionView;
  useStore.setState({ focusedId: 'test', sessions: { test: session }, browserPages, browserNavigate, setBrowserSub: vi.fn(), listSnapshots: vi.fn(async () => []) });
  const view = render(<BrowserPane />);
  const address = view.getByLabelText('Browser URL') as HTMLInputElement;
  await waitFor(() => expect(address.value).toBe('https://example.test'));
  expect(address.readOnly).toBe(true);
  expect((view.getByLabelText('Back') as HTMLButtonElement).disabled).toBe(true);
  fireEvent.click(view.getByRole('tab', { name: 'Second' }));
  await waitFor(() => expect(address.value).toBe('about:blank'));
  expect(browserPages).toHaveBeenCalledWith('test', 'second');
  expect(browserNavigate).not.toHaveBeenCalled();

  useStore.setState({ sessions: { test: { ...session, browserOwner: 'user' } } });
  await waitFor(() => expect(address.readOnly).toBe(false));
  fireEvent.focus(address);
  fireEvent.change(address, { target: { value: 'localhost:3000' } });
  fireEvent.blur(address);
  fireEvent.click(view.getByRole('button', { name: 'Go' }));
  await waitFor(() => expect(browserNavigate).toHaveBeenCalledWith('test', { kind: 'navigate', url: 'http://localhost:3000' }));
});
