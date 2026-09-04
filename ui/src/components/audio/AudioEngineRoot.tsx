import { useEffect, useRef } from 'react';
import { bindElement, installMediaSession, setPositionSender } from '../../audio/engine';
import { useStore } from '../../store';

// The single, permanently-mounted <audio> element every chat's playback runs
// through (see ui/src/audio/engine.ts). Mounted once at the app root so it
// survives row unmounts and chat switches — critical on iOS, where a
// newly-created element cannot autoplay without its own fresh user gesture.
// No native controls: InlineAudioBar / GlobalAudioPlayer are the UI.
export function AudioEngineRoot() {
  const ref = useRef<HTMLAudioElement>(null);
  const setAudioPosition = useStore((s) => s.setAudioPosition);

  useEffect(() => {
    if (ref.current) bindElement(ref.current);
    installMediaSession();
    // The engine stays framework/store-agnostic (see engine.ts's file
    // header); this is the one place that hands it a way to reach the WS
    // client for the durable half of position restore.
    setPositionSender(setAudioPosition);
    return () => setPositionSender(null);
  }, [setAudioPosition]);

  return <audio ref={ref} data-testid="audio-engine-element" preload="metadata" style={{ display: 'none' }} />;
}
