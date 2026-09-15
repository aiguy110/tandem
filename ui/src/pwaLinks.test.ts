import { afterEach, describe, expect, it, vi } from 'vitest';
import { installPWAExternalLinkHandler, isRunningAsPWA } from './pwaLinks';

function setStandalone(matches: boolean) {
  vi.stubGlobal('matchMedia', vi.fn().mockReturnValue({ matches }));
}

function addLink(href: string, attributes = ''): HTMLAnchorElement {
  document.body.innerHTML = `<a href="${href}" ${attributes}><span>Open</span></a>`;
  return document.querySelector('a')!;
}

describe('PWA external links', () => {
  afterEach(() => {
    document.body.replaceChildren();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('recognizes standalone display mode', () => {
    setStandalone(true);
    expect(isRunningAsPWA()).toBe(true);
  });

  it('opens an ordinary external link outside the PWA', () => {
    setStandalone(true);
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    const link = addLink('https://example.com/docs');
    const remove = installPWAExternalLinkHandler();

    const event = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 });
    link.querySelector('span')!.dispatchEvent(event);

    expect(event.defaultPrevented).toBe(true);
    expect(open).toHaveBeenCalledWith('https://example.com/docs', '_blank', 'noopener');
    remove();
  });

  it.each([
    ['normal browser', false, '', {}],
    ['same-origin link', true, 'http://localhost:3000/settings', {}],
    ['Ctrl-click', true, 'https://example.com', { ctrlKey: true }],
    ['explicit target', true, 'https://example.com', {}, 'target="_blank"'],
    ['download link', true, 'https://example.com/file', {}, 'download'],
  ])('leaves %s to normal browser behavior', (_name, standalone, href, init, attributes = '') => {
    setStandalone(standalone);
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    const link = addLink(href, attributes);
    const remove = installPWAExternalLinkHandler();
    // Let jsdom avoid attempting the normal navigation after our handler has
    // made its decision. This listener is registered later, so it does not
    // mask whether the handler would have opened the link.
    document.addEventListener('click', (event) => event.preventDefault(), { once: true });

    const event = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0, ...init });
    link.dispatchEvent(event);

    expect(open).not.toHaveBeenCalled();
    remove();
  });
});
