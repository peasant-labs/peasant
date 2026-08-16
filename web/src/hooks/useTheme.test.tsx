import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { render, screen, cleanup, fireEvent } from '@testing-library/react';
import { useTheme } from './useTheme';

/**
 * Two INDEPENDENT consumers, the way the real app mounts them: the navbar owns
 * the toggle, and the session viewer reads the theme to pass down as a prop.
 */
function Toggler() {
  const { toggle } = useTheme();
  return <button onClick={toggle}>toggle</button>;
}

function Reader() {
  const { theme } = useTheme();
  return <span data-testid="reader">{theme}</span>;
}

function App() {
  return (
    <>
      <Toggler />
      <Reader />
    </>
  );
}

describe('useTheme', () => {
  beforeEach(() => {
    window.localStorage.clear();
    document.documentElement.removeAttribute('data-theme');
    document.documentElement.removeAttribute('data-tb-theme');
  });
  afterEach(cleanup);

  // The regression: with per-component state, toggling in one consumer left
  // every other consumer on its own stale value, so anything passing the theme
  // DOWN AS A PROP (the transcript viewer) never re-themed.
  it('keeps separate consumers on the same value across a toggle', () => {
    render(<App />);
    expect(screen.getByTestId('reader')).toHaveTextContent('dark');

    fireEvent.click(screen.getByText('toggle'));
    expect(screen.getByTestId('reader')).toHaveTextContent('light');

    fireEvent.click(screen.getByText('toggle'));
    expect(screen.getByTestId('reader')).toHaveTextContent('dark');
  });

  it('drives both DOM attributes, which is what CSS-styled chrome follows', () => {
    render(<App />);
    fireEvent.click(screen.getByText('toggle'));

    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
    expect(document.documentElement.getAttribute('data-tb-theme')).toBe('light');
  });

  it('persists the choice and restores it on the next mount', () => {
    render(<App />);
    fireEvent.click(screen.getByText('toggle'));
    expect(window.localStorage.getItem('peasant-theme')).toBe('light');

    cleanup();
    render(<App />);
    expect(screen.getByTestId('reader')).toHaveTextContent('light');
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
  });

  it('treats a garbage stored value as the dark default', () => {
    window.localStorage.setItem('peasant-theme', 'chartreuse');
    render(<App />);
    expect(screen.getByTestId('reader')).toHaveTextContent('dark');
  });
});
