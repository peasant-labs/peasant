import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { useRef, useState } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ALL_FEEDBACK_KINDS, FeedbackKind } from '@/lib/feedback/types';
import {
  parseStrictYAML,
  requireExactFields,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';
import { FeedbackLauncher } from './FeedbackLauncher';

const FIXTURE_PATH = resolve(process.cwd(), 'src/components/feedback/testdata/launcher_menu_cases.yaml');

const REQUIRED_NAMES = [
  'click-toggles-radial-menu',
  'arrow-keys-cycle-both-tiles',
  'escape-closes-and-restores-focus',
  'enter-invokes-selected-kind',
  'space-invokes-selected-kind',
  'home-end-jump-tiles',
  'click-outside-closes',
] as const;

type KindKey = 'problem' | 'feature';
type FocusTarget = KindKey | 'launcher';

type Step =
  | { clickTrigger: true }
  | { clickOutside: true }
  | { key: string }
  | { expectOpen: boolean }
  | { expectActiveKind: KindKey }
  | { expectFocused: FocusTarget }
  | { expectSelected: KindKey }
  | { expectTabIndex: { problem: number; feature: number } };

const STEP_KEYS = [
  'clickTrigger',
  'clickOutside',
  'key',
  'expectOpen',
  'expectActiveKind',
  'expectFocused',
  'expectSelected',
  'expectTabIndex',
] as const;

interface LauncherCase {
  name: string;
  initialOpen: boolean;
  steps: Step[];
}

function kindFromKey(key: KindKey): FeedbackKind {
  return key === 'problem' ? FeedbackKind.Problem : FeedbackKind.Feature;
}

function loadFixture(): LauncherCase[] {
  const root = requireRecord(parseStrictYAML(readFileSync(FIXTURE_PATH, 'utf8'), 'launcher menu fixture'), 'launcher menu fixture');
  requireExactRequiredFields(root, ['requiredNames', 'cases'], 'launcher menu fixture');
  const requiredNames = root.requiredNames;
  if (
    !Array.isArray(requiredNames) ||
    requiredNames.length === 0 ||
    requiredNames.some((n) => typeof n !== 'string' || n.length === 0) ||
    new Set(requiredNames).size !== requiredNames.length
  ) {
    throw new Error('launcher menu fixture.requiredNames must be unique non-empty strings');
  }
  if (!Array.isArray(root.cases) || root.cases.length === 0) {
    throw new Error('launcher menu fixture.cases must be a non-empty array');
  }
  const rows = root.cases.map((row, index) => requireRecord(row, `launcher menu fixture.cases[${index}]`));
  requireUniqueNames(rows, 'launcher menu fixture.cases');

  const names = rows.map((r) => r.name as string);
  const missingFromFixture = REQUIRED_NAMES.filter((name) => !names.includes(name));
  const missingFromManifest = (requiredNames as string[]).filter((name) => !names.includes(name));
  const undeclared = names.filter((name) => !(requiredNames as string[]).includes(name));
  if (missingFromFixture.length || missingFromManifest.length || undeclared.length) {
    throw new Error(
      `launcher menu fixture names mismatch (missing: ${[...missingFromFixture, ...missingFromManifest].join(', ') || 'none'}; undeclared: ${undeclared.join(', ') || 'none'})`,
    );
  }

  return rows.map((row, index) => {
    requireExactRequiredFields(row, ['name', 'initialOpen', 'steps'], `cases[${index}]`);
    if (typeof row.initialOpen !== 'boolean' || !Array.isArray(row.steps) || row.steps.length === 0) {
      throw new Error(`cases[${index}] has invalid initialOpen or steps`);
    }
    const steps = row.steps.map((rawStep, stepIndex) => {
      const step = requireRecord(rawStep, `cases[${index}].steps[${stepIndex}]`);
      requireExactFields(step, STEP_KEYS, `cases[${index}].steps[${stepIndex}]`);
      if (Object.keys(step).length !== 1) {
        throw new Error(`cases[${index}].steps[${stepIndex}] must hold exactly one action or assertion`);
      }
      return step as Step;
    });
    return {
      name: row.name as string,
      initialOpen: row.initialOpen,
      steps,
    };
  });
}

const CASES = loadFixture();

function Harness({
  initialOpen,
  onSelectKind,
  disabled,
}: {
  initialOpen: boolean;
  onSelectKind: (kind: FeedbackKind) => void;
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(initialOpen);
  const launcherRef = useRef<HTMLButtonElement | null>(null);
  return (
    <div>
      <button type="button" data-testid="outside">
        outside
      </button>
      <FeedbackLauncher
        open={open}
        onOpenChange={setOpen}
        onSelectKind={onSelectKind}
        disabled={disabled}
        launcherRef={launcherRef}
      />
    </div>
  );
}

describe('FeedbackLauncher', () => {
  beforeEach(() => {
    vi.spyOn(window, 'requestAnimationFrame').mockImplementation((cb) => {
      cb(0);
      return 1;
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
    cleanup();
  });

  it.each(CASES)('$name', (c) => {
    const onSelectKind = vi.fn();
    const { container } = render(<Harness initialOpen={c.initialOpen} onSelectKind={onSelectKind} />);

    const trigger = () => screen.getByRole('button', { name: 'feedback' });
    const itemFor = (key: KindKey) => screen.getByRole('menuitem', { name: kindFromKey(key).label });

    for (const step of c.steps) {
      if ('clickTrigger' in step) {
        act(() => {
          fireEvent.click(trigger());
        });
      } else if ('clickOutside' in step) {
        act(() => {
          fireEvent.pointerDown(screen.getByTestId('outside'));
        });
      } else if ('key' in step) {
        const active = document.activeElement ?? trigger();
        act(() => {
          fireEvent.keyDown(active, { key: step.key });
        });
      } else if ('expectOpen' in step) {
        expect(trigger()).toHaveAttribute('aria-expanded', String(step.expectOpen));
        if (step.expectOpen) {
          expect(screen.getByRole('menu')).toBeInTheDocument();
        } else {
          expect(screen.queryByRole('menu')).toBeNull();
        }
      } else if ('expectActiveKind' in step) {
        expect(itemFor(step.expectActiveKind)).toHaveAttribute('tabindex', '0');
      } else if ('expectFocused' in step) {
        const expectedEl = step.expectFocused === 'launcher' ? trigger() : itemFor(step.expectFocused);
        expect(document.activeElement).toBe(expectedEl);
      } else if ('expectSelected' in step) {
        expect(onSelectKind).toHaveBeenCalledWith(kindFromKey(step.expectSelected));
      } else if ('expectTabIndex' in step) {
        expect(itemFor('problem')).toHaveAttribute('tabindex', String(step.expectTabIndex.problem));
        expect(itemFor('feature')).toHaveAttribute('tabindex', String(step.expectTabIndex.feature));
      }
    }

    expect(container.querySelector('[data-feedback-widget]')).not.toBeNull();
    expect(container.querySelectorAll('a[href]')).toHaveLength(0);
  });

  it('exposes WAI-ARIA menu-button attributes and renders all feedback kinds from ALL_FEEDBACK_KINDS', () => {
    const onOpenChange = vi.fn();
    const onSelectKind = vi.fn();
    const { container } = render(
      <FeedbackLauncher open onOpenChange={onOpenChange} onSelectKind={onSelectKind} />,
    );

    const trigger = screen.getByRole('button', { name: 'feedback' });
    const menu = screen.getByRole('menu');
    expect(trigger).toHaveAttribute('aria-haspopup', 'menu');
    expect(trigger).toHaveAttribute('aria-expanded', 'true');
    expect(trigger).toHaveAttribute('aria-controls', menu.id);

    const items = screen.getAllByRole('menuitem');
    expect(items.map((el) => el.textContent)).toEqual(ALL_FEEDBACK_KINDS.map((k) => k.label));
    expect(container.querySelectorAll('a[href]')).toHaveLength(0);
  });

  it('respects disabled prop on the launcher trigger', () => {
    const onOpenChange = vi.fn();
    const onSelectKind = vi.fn();
    render(<FeedbackLauncher open={false} onOpenChange={onOpenChange} onSelectKind={onSelectKind} disabled />);

    const trigger = screen.getByRole('button', { name: 'feedback' });
    expect(trigger).toBeDisabled();
    fireEvent.click(trigger);
    fireEvent.keyDown(trigger, { key: 'ArrowDown' });
    expect(onOpenChange).not.toHaveBeenCalled();
  });
});
