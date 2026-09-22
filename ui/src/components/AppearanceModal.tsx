import { useEffect, useRef } from 'react';
import { useStore } from '../store';
import {
  DEFAULT_APPEARANCE,
  TERM_FONT_RANGE,
  UI_FONT_RANGE,
  setLinked,
  setTermFontSize,
  setUiFontSize,
} from '../appearance';
import { selectedFontFamily } from '../terminal/font';

interface SizeRowProps {
  id: string;
  label: string;
  hint: string;
  value: number;
  range: { min: number; max: number; step: number };
  onChange: (px: number) => void;
}

function SizeRow({ id, label, hint, value, range, onChange }: SizeRowProps) {
  return (
    <div className="appearance-size">
      <label className="appearance-label" htmlFor={id}>
        <span className="primary">{label}</span>
        <span className="sub">{hint}</span>
      </label>
      <div className="appearance-stepper">
        <button type="button" className="btn ghost" aria-label={`Decrease ${label}`} disabled={value <= range.min} onClick={() => onChange(value - range.step)}>−</button>
        <input
          id={id}
          type="range"
          min={range.min}
          max={range.max}
          step={range.step}
          value={value}
          onChange={(e) => onChange(Number(e.target.value))}
        />
        <button type="button" className="btn ghost" aria-label={`Increase ${label}`} disabled={value >= range.max} onClick={() => onChange(value + range.step)}>+</button>
        <output className="appearance-value" htmlFor={id}>{value}px</output>
      </div>
    </div>
  );
}

export function AppearanceModal() {
  const setModal = useStore((s) => s.setModal);
  const theme = useStore((s) => s.theme);
  const setTheme = useStore((s) => s.setTheme);
  const appearance = useStore((s) => s.appearance);
  const setAppearance = useStore((s) => s.setAppearance);
  const firstRef = useRef<HTMLButtonElement>(null);

  useEffect(() => firstRef.current?.focus(), []);

  const close = () => setModal('none');
  const isDefault =
    appearance.uiFontSize === DEFAULT_APPEARANCE.uiFontSize &&
    appearance.termFontSize === DEFAULT_APPEARANCE.termFontSize &&
    appearance.linked === DEFAULT_APPEARANCE.linked;

  return (
    <div className="modal-scrim" onMouseDown={(e) => e.target === e.currentTarget && close()}>
      <div className="modal appearance-modal" role="dialog" aria-modal="true" aria-labelledby="appearance-title" onKeyDown={(e) => e.key === 'Escape' && close()}>
        <div className="automation-header">
          <div>
            <div className="primary" id="appearance-title">Appearance</div>
            <div className="sub">Saved in this browser.</div>
          </div>
          <button type="button" className="automation-close" onClick={close} aria-label="Close appearance">×</button>
        </div>

        <section className="appearance-section">
          <div className="appearance-heading">Theme</div>
          <div className="appearance-segmented" role="radiogroup" aria-label="Theme">
            {(['dark', 'light'] as const).map((t, i) => (
              <button
                key={t}
                ref={i === 0 ? firstRef : undefined}
                type="button"
                role="radio"
                aria-checked={theme === t}
                className={theme === t ? 'on' : undefined}
                onClick={() => setTheme(t)}
              >
                {t === 'dark' ? '◐ Dark' : '◑ Light'}
              </button>
            ))}
          </div>
        </section>

        <section className="appearance-section">
          <div className="appearance-heading">Font size</div>
          <SizeRow
            id="appearance-ui-size"
            label="App"
            hint="Rails, transcript, palettes"
            value={appearance.uiFontSize}
            range={UI_FONT_RANGE}
            onChange={(px) => setAppearance(setUiFontSize(appearance, px))}
          />
          <div className="appearance-link-row">
            <button
              type="button"
              className={`appearance-link${appearance.linked ? ' on' : ''}`}
              aria-pressed={appearance.linked}
              onClick={() => setAppearance(setLinked(appearance, !appearance.linked))}
              title={appearance.linked ? 'Sizes move together. Click to adjust separately.' : 'Sizes are independent. Click to lock them together.'}
            >
              {appearance.linked ? '🔒 Locked together' : '🔓 Adjusted separately'}
            </button>
          </div>
          <SizeRow
            id="appearance-term-size"
            label="Terminal"
            hint="Chat CLI and Terminal panes"
            value={appearance.termFontSize}
            range={TERM_FONT_RANGE}
            onChange={(px) => setAppearance(setTermFontSize(appearance, px))}
          />
          <div className="appearance-preview" aria-hidden="true">
            <div style={{ fontSize: 'var(--fs-base)' }}>The quick brown fox jumps over the lazy dog.</div>
            <pre style={{ fontFamily: selectedFontFamily(), fontSize: `${appearance.termFontSize}px` }}>
              {' ~/tandem   master  ❯ ls -la'}
            </pre>
          </div>
        </section>

        <div className="foot">
          <button type="button" className="btn ghost" disabled={isDefault} onClick={() => setAppearance(DEFAULT_APPEARANCE)}>Reset to defaults</button>
        </div>
      </div>
    </div>
  );
}
