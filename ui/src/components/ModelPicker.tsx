import type React from 'react';
import { useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from 'react';

// Up/Down on a focused select steps through its options (wrapping) and
// commits immediately, matching the ModelPicker. Browsers disagree on what
// arrow keys do to a closed <select> (macOS opens the popup), so do it here.
export function cycleSelectOnArrow(event: React.KeyboardEvent<HTMLSelectElement>, onChange: (value: string) => void) {
  if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return;
  const select = event.currentTarget;
  if (select.options.length === 0) return;
  event.preventDefault();
  event.stopPropagation();
  const delta = event.key === 'ArrowDown' ? 1 : -1;
  const next = (select.selectedIndex + delta + select.options.length) % select.options.length;
  onChange(select.options[next].value);
}

export interface ModelOption {
  value: string;
  name: string;
  description?: string | null;
}

function providerOf(option: ModelOption): string {
  return option.value.split('/', 1)[0] || 'other';
}

function displayName(option: ModelOption): string {
  const prefix = `${providerOf(option)}/`;
  return option.name.startsWith(prefix) ? option.name.slice(prefix.length) : option.name;
}

// ACP agents can return hundreds of models (notably OpenRouter). A native
// select makes that catalogue technically available but impractical to browse.
// Keep the raw ACP value intact while giving it a searchable, provider-grouped
// control that works identically in spawn and an active session.
export function ModelPicker({
  value,
  options,
  onChange,
  disabled = false,
  defaultLabel,
}: {
  value: string;
  options: ModelOption[];
  onChange: (value: string) => void;
  disabled?: boolean;
  defaultLabel?: string;
}) {
  const id = useId();
  const root = useRef<HTMLDivElement>(null);
  const input = useRef<HTMLInputElement>(null);
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const [placement, setPlacement] = useState<{ up: boolean; maxHeight: number }>({ up: false, maxHeight: 440 });
  const selected = options.find((option) => option.value === value);
  const shownValue = selected?.name ?? (value || defaultLabel || 'Select model');

  useEffect(() => {
    const close = (event: MouseEvent) => {
      if (!root.current?.contains(event.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', close);
    return () => document.removeEventListener('mousedown', close);
  }, []);

  useEffect(() => {
    if (!open) setQuery('');
  }, [open]);

  // The session config bar sits at the bottom of the pane, so a menu that
  // always drops down runs off-screen. Open toward whichever side has room,
  // preferring below, and cap the height to the space actually available.
  useLayoutEffect(() => {
    if (!open || !input.current) return;
    const place = () => {
      const rect = input.current!.getBoundingClientRect();
      const wanted = Math.min(440, window.innerHeight * 0.6);
      const below = window.innerHeight - rect.bottom - 12;
      const above = rect.top - 12;
      const up = below < wanted && above > below;
      setPlacement({ up, maxHeight: Math.max(120, Math.min(wanted, up ? above : below)) });
    };
    place();
    window.addEventListener('resize', place);
    return () => window.removeEventListener('resize', place);
  }, [open]);

  const grouped = useMemo(() => {
    const needle = query.trim().toLocaleLowerCase();
    const matching = needle
      ? options.filter((option) => `${option.value} ${option.name} ${option.description ?? ''}`.toLocaleLowerCase().replace(/[-_/:]+/g, ' ').includes(needle))
      : options;
    const groups = new Map<string, ModelOption[]>();
    for (const option of matching) {
      const provider = providerOf(option);
      const items = groups.get(provider) ?? [];
      items.push(option);
      groups.set(provider, items);
    }
    return [...groups.entries()].sort(([a], [b]) => a.localeCompare(b));
  }, [options, query]);
  const matches = useMemo(() => grouped.flatMap(([, items]) => items), [grouped]);
  const [active, setActive] = useState(0);
  const menu = useRef<HTMLDivElement>(null);

  useEffect(() => setActive(0), [query, open]);

  useEffect(() => {
    if (!open) return;
    menu.current?.querySelector('.model-picker-active')?.scrollIntoView?.({ block: 'nearest' });
  }, [active, open]);

  const move = (delta: number) => {
    if (matches.length === 0) return;
    setActive((index) => (index + delta + matches.length) % matches.length);
  };

  const choose = (next: string) => {
    onChange(next);
    setOpen(false);
    input.current?.blur();
  };

  return (
    <div className="model-picker" ref={root}>
      <input
        ref={input}
        id={id}
        role="combobox"
        aria-autocomplete="list"
        aria-controls={`${id}-models`}
        aria-expanded={open}
        aria-label="Model"
        value={open ? query : shownValue}
        placeholder={defaultLabel || 'Search models…'}
        disabled={disabled}
        readOnly={!open}
        onFocus={() => { setOpen(true); setQuery(''); }}
        onChange={(event) => { setOpen(true); setQuery(event.target.value); }}
        onKeyDown={(event) => {
          if (event.key === 'Escape') { setOpen(false); input.current?.blur(); }
          if (event.key === 'Enter' && matches[active]) { event.preventDefault(); choose(matches[active].value); }
          if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
            event.preventDefault();
            if (!open) setOpen(true);
            else move(event.key === 'ArrowDown' ? 1 : -1);
          }
        }}
      />
      {open && (
        <div id={`${id}-models`} className={`model-picker-menu${placement.up ? ' model-picker-menu-up' : ''}`} style={{ maxHeight: placement.maxHeight }} ref={menu} role="listbox" aria-label="Available models">
          {defaultLabel && (
            <button type="button" role="option" aria-selected={!value} className="model-picker-option model-picker-default" onMouseDown={(event) => event.preventDefault()} onClick={() => choose('')}>
              {defaultLabel}
            </button>
          )}
          {grouped.map(([provider, items]) => (
            <div className="model-picker-group" key={provider}>
              <div className="model-picker-provider">{provider}</div>
              {items.map((option) => (
                <button
                  type="button"
                  role="option"
                  aria-selected={option.value === value}
                  className={`model-picker-option${option === matches[active] ? ' model-picker-active' : ''}`}
                  key={option.value}
                  title={option.description ?? option.name}
                  onMouseDown={(event) => event.preventDefault()}
                  onClick={() => choose(option.value)}
                >
                  <span className="model-picker-name">{displayName(option)}</span>
                  {option.description && <small>{option.description}</small>}
                </button>
              ))}
            </div>
          ))}
          {grouped.length === 0 && <div className="model-picker-empty">No matching models</div>}
        </div>
      )}
    </div>
  );
}
