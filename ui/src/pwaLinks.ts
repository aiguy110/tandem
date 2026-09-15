// A standalone PWA has no browser chrome, so following an external link in
// the app's window strands the user away from Tandem. Open those destinations
// in the browser instead, but leave ordinary browser link gestures untouched.
const STANDALONE_DISPLAY_MODE = '(display-mode: standalone)';

interface StandaloneNavigator extends Navigator {
  standalone?: boolean;
}

export function isRunningAsPWA(): boolean {
  return window.matchMedia?.(STANDALONE_DISPLAY_MODE).matches === true
    || (navigator as StandaloneNavigator).standalone === true;
}

function isExternalWebLink(anchor: HTMLAnchorElement): boolean {
  const url = new URL(anchor.href, window.location.href);
  return (url.protocol === 'http:' || url.protocol === 'https:')
    && url.origin !== window.location.origin;
}

function shouldOpenInBrowser(event: MouseEvent, anchor: HTMLAnchorElement): boolean {
  return !event.defaultPrevented
    && event.button === 0
    && !event.metaKey
    && !event.ctrlKey
    && !event.shiftKey
    && !event.altKey
    && !anchor.hasAttribute('download')
    && !anchor.hasAttribute('target')
    && isExternalWebLink(anchor);
}

/** Installs PWA-only external-link handling and returns an uninstall function. */
export function installPWAExternalLinkHandler(): () => void {
  if (!isRunningAsPWA()) return () => {};

  const onClick = (event: MouseEvent) => {
    const target = event.target;
    if (!(target instanceof Element)) return;
    const anchor = target.closest<HTMLAnchorElement>('a[href]');
    if (!anchor || !shouldOpenInBrowser(event, anchor)) return;

    event.preventDefault();
    window.open(anchor.href, '_blank', 'noopener');
  };

  document.addEventListener('click', onClick);
  return () => document.removeEventListener('click', onClick);
}
