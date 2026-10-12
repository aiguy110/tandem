import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ModelPicker } from './ModelPicker';

const options = [
  { value: 'openrouter/moonshotai/kimi-k2.5', name: 'openrouter/moonshotai/kimi-k2.5' },
  { value: 'openrouter/qwen/qwen3-coder-next', name: 'openrouter/qwen/qwen3-coder-next', description: 'Coding agent' },
  { value: 'openai/gpt-5.6-terra', name: 'openai/gpt-5.6-terra' },
];

afterEach(cleanup);

describe('ModelPicker', () => {
  it('searches across the complete model catalog and chooses an exact ACP value', () => {
    const onChange = vi.fn();
    render(<ModelPicker value="" options={options} onChange={onChange} defaultLabel="Agent default" />);

    const input = screen.getByRole('combobox', { name: 'Model' });
    fireEvent.focus(input);
    expect(screen.getByText('openrouter')).toBeTruthy();
    expect(screen.getByText('openai')).toBeTruthy();

    fireEvent.change(input, { target: { value: 'coder next' } });
    expect(screen.getByText('qwen/qwen3-coder-next')).toBeTruthy();
    expect(screen.queryByText('moonshotai/kimi-k2.5')).toBeNull();

    fireEvent.click(screen.getByRole('option', { name: /qwen\/qwen3-coder-next/i }));
    expect(onChange).toHaveBeenCalledWith('openrouter/qwen/qwen3-coder-next');
  });

  it('keeps Agent default selectable for a spawn form', () => {
    const onChange = vi.fn();
    render(<ModelPicker value="openai/gpt-5.6-terra" options={options} onChange={onChange} defaultLabel="Agent default" />);
    fireEvent.focus(screen.getByRole('combobox', { name: 'Model' }));
    fireEvent.click(screen.getByRole('option', { name: 'Agent default' }));
    expect(onChange).toHaveBeenCalledWith('');
  });
});
