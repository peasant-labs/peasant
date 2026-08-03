import { describe, it, expect, beforeEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { Explainer, ExplainerToggle, useExplainer } from './Explainer';

function Harness({ id }: { id: string }) {
  const explainer = useExplainer(id);
  return (
    <div>
      <ExplainerToggle explainer={explainer} />
      <Explainer explainer={explainer} title="what am I looking at?">
        <p>The body copy.</p>
      </Explainer>
    </div>
  );
}

describe('Explainer', () => {
  beforeEach(() => {
    window.localStorage.clear();
  });

  it('is collapsed by default; opens from the ? toggle and hides on ×', () => {
    render(<Harness id="t1" />);
    // Collapsed: the payload leads, the body is hidden behind the ? toggle.
    expect(screen.queryByText('The body copy.')).not.toBeInTheDocument();
    const toggle = screen.getByLabelText('what am I looking at?');
    expect(toggle).toBeInTheDocument();

    // Open it from the toggle.
    fireEvent.click(toggle);
    expect(screen.getByText('The body copy.')).toBeInTheDocument();
    // No ? toggle while open.
    expect(screen.queryByLabelText('what am I looking at?')).not.toBeInTheDocument();

    // Hide via ×.
    fireEvent.click(screen.getByLabelText('Hide explanation'));
    expect(screen.queryByText('The body copy.')).not.toBeInTheDocument();
  });

  it('persists an opened explainer across remounts via localStorage', () => {
    const { unmount } = render(<Harness id="t2" />);
    fireEvent.click(screen.getByLabelText('what am I looking at?')); // opt in
    expect(screen.getByText('The body copy.')).toBeInTheDocument();
    unmount();

    render(<Harness id="t2" />);
    // Stays open — the user explicitly opted in.
    expect(screen.getByText('The body copy.')).toBeInTheDocument();
  });

  it('spans full content width and animates in when open', () => {
    render(<Harness id="w1" />);
    fireEvent.click(screen.getByLabelText('what am I looking at?'));
    // The box is a labeled region (so it can be placed full-width anywhere) that
    // fills its container and carries the entrance animation.
    const region = screen.getByRole('region', { name: 'Explanation' });
    expect(region).toHaveClass('w-full', 'block', 'animate-explainer-in');
  });

  it('keeps two surfaces independent', () => {
    render(
      <div>
        <Harness id="a" />
        <Harness id="b" />
      </div>,
    );
    // Both start collapsed → two ? toggles.
    const toggles = screen.getAllByLabelText('what am I looking at?');
    expect(toggles).toHaveLength(2);

    // Open only the first.
    fireEvent.click(toggles[0]);
    expect(screen.getAllByText('The body copy.')).toHaveLength(1);
  });
});
