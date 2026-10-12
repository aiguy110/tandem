import { useEffect, useId, useMemo, useRef, useState } from 'react';

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
  const first = grouped[0]?.[1][0];

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
          if (event.key === 'Enter' && first) { event.preventDefault(); choose(first.value); }
          if (event.key === 'ArrowDown') setOpen(true);
        }}
      />
      {open && (
        <div id={`${id}-models`} className="model-picker-menu" role="listbox" aria-label="Available models">
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
                  className="model-picker-option"
                  key={option.value}
                  title={option.description ?? option.name}
                  onMouseDown={(event) => event.preventDefault()}
                  onClick={() => choose(option.value)}
                >
                  <span>{displayName(option)}</span>
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
