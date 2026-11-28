import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { closed } from '@peasant-labs/fairtrade/closed-value';
import {
  parseStrictYAML,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';
import { ALL_FEEDBACK_KINDS, type FeedbackKind } from '@/lib/feedback/types';
import { AttachOptionsDialog, AttachOptionsLabels } from './AttachOptionsDialog';

const FIXTURE_PATH = 'src/components/feedback/testdata/attach_options_cases.yaml';

const AttachAction = Object.freeze({
  None: closed('none', 'none'),
  ToggleElement: closed('toggle_element', 'toggle element'),
  TogglePage: closed('toggle_page', 'toggle page'),
  Proceed: closed('proceed', 'proceed'),
  Cancel: closed('cancel', 'cancel'),
  Escape: closed('escape', 'escape'),
} as const);
type AttachAction = (typeof AttachAction)[keyof typeof AttachAction];

const ALL_ATTACH_ACTIONS = Object.freeze([
  AttachAction.None,
  AttachAction.ToggleElement,
  AttachAction.TogglePage,
  AttachAction.Proceed,
  AttachAction.Cancel,
  AttachAction.Escape,
] as const);

interface AttachCase {
  name: string;
  kind: FeedbackKind;
  expectedTitle: string;
  attachElement: boolean;
  attachPage: boolean;
  action: AttachAction;
}

function loadCases(): AttachCase[] {
  const root = requireRecord(
    parseStrictYAML(readFileSync(resolve(process.cwd(), FIXTURE_PATH), 'utf8'), 'attach options fixture'),
    'attach options fixture',
  );
  requireExactRequiredFields(root, ['requiredNames', 'cases'], 'attach options fixture');
  const requiredNames = root.requiredNames;
  if (
    !Array.isArray(requiredNames) ||
    requiredNames.length === 0 ||
    requiredNames.some((n) => typeof n !== 'string' || n.length === 0) ||
    new Set(requiredNames).size !== requiredNames.length
  ) {
    throw new Error('attach options fixture.requiredNames must be unique non-empty strings');
  }
  if (!Array.isArray(root.cases) || root.cases.length === 0) {
    throw new Error('attach options fixture.cases must be a non-empty array');
  }
  const rows = root.cases.map((row, index) => requireRecord(row, `attach options fixture.cases[${index}]`));
  requireUniqueNames(rows, 'attach options fixture.cases');

  const names = rows.map((row) => row.name as string);
  const missing = (requiredNames as string[]).filter((n) => !names.includes(n));
  const undeclared = names.filter((n) => !(requiredNames as string[]).includes(n));
  if (missing.length > 0 || undeclared.length > 0) {
    throw new Error(
      `attach options fixture cases mismatch requiredNames (missing: ${missing.join(', ') || 'none'}; undeclared: ${undeclared.join(', ') || 'none'})`,
    );
  }

  return rows.map((row, index) => {
    requireExactRequiredFields(
      row,
      ['name', 'kind', 'expectedTitle', 'attachElement', 'attachPage', 'action'],
      `cases[${index}]`,
    );
    const kind = ALL_FEEDBACK_KINDS.find((k) => k.wire === row.kind);
    if (!kind) throw new Error(`cases[${index}].kind is unknown: ${String(row.kind)}`);
    const action = ALL_ATTACH_ACTIONS.find((a) => a.wire === row.action);
    if (
      typeof row.expectedTitle !== 'string' ||
      typeof row.attachElement !== 'boolean' ||
      typeof row.attachPage !== 'boolean' ||
      !action
    ) {
      throw new Error(`cases[${index}] has invalid field types`);
    }
    return {
      name: row.name as string,
      kind,
      expectedTitle: row.expectedTitle,
      attachElement: row.attachElement,
      attachPage: row.attachPage,
      action,
    };
  });
}

const CASES = loadCases();

describe('AttachOptionsDialog', () => {
  afterEach(() => {
    cleanup();
  });

  it.each(CASES)('$name', (c) => {
    const onToggleElement = vi.fn();
    const onTogglePage = vi.fn();
    const onProceed = vi.fn();
    const onClose = vi.fn();

    render(
      <AttachOptionsDialog
        open
        kind={c.kind}
        attachElement={c.attachElement}
        attachPage={c.attachPage}
        onToggleElement={onToggleElement}
        onTogglePage={onTogglePage}
        onProceed={onProceed}
        onClose={onClose}
      />,
    );

    const dialog = screen.getByRole('dialog', { name: c.expectedTitle });
    expect(dialog).toBeInTheDocument();

    const elementCheckbox = screen.getByRole('checkbox', { name: AttachOptionsLabels.PickElement }) as HTMLInputElement;
    const pageCheckbox = screen.getByRole('checkbox', { name: AttachOptionsLabels.CapturePage }) as HTMLInputElement;
    expect(elementCheckbox.checked).toBe(c.attachElement);
    expect(pageCheckbox.checked).toBe(c.attachPage);
    expect(screen.getByText(AttachOptionsLabels.MaskNotice)).toBeInTheDocument();

    if (c.action === AttachAction.ToggleElement) {
      fireEvent.click(elementCheckbox);
      expect(onToggleElement).toHaveBeenCalledTimes(1);
      expect(onTogglePage).not.toHaveBeenCalled();
    } else if (c.action === AttachAction.TogglePage) {
      fireEvent.click(pageCheckbox);
      expect(onTogglePage).toHaveBeenCalledTimes(1);
      expect(onToggleElement).not.toHaveBeenCalled();
    } else if (c.action === AttachAction.Proceed) {
      fireEvent.click(screen.getByRole('button', { name: AttachOptionsLabels.Continue }));
      expect(onProceed).toHaveBeenCalledTimes(1);
    } else if (c.action === AttachAction.Cancel) {
      fireEvent.click(screen.getByRole('button', { name: AttachOptionsLabels.Cancel }));
      expect(onClose).toHaveBeenCalledTimes(1);
    } else if (c.action === AttachAction.Escape) {
      fireEvent.keyDown(document, { key: 'Escape' });
      expect(onClose).toHaveBeenCalledTimes(1);
    }
  });

  it('renders nothing when closed', () => {
    render(
      <AttachOptionsDialog
        open={false}
        kind={ALL_FEEDBACK_KINDS[0]}
        attachElement={false}
        attachPage={false}
        onToggleElement={vi.fn()}
        onTogglePage={vi.fn()}
        onProceed={vi.fn()}
        onClose={vi.fn()}
      />,
    );
    expect(screen.queryByRole('dialog')).toBeNull();
  });
});
