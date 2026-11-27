import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import {
  parseStrictYAML,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';
import { ALL_FEEDBACK_KINDS, type FeedbackKind } from '@/lib/feedback/types';
import { AttachOptionsDialog } from './AttachOptionsDialog';

type AttachAction = 'none' | 'toggle_element' | 'toggle_page' | 'proceed' | 'cancel' | 'escape';

interface AttachCase {
  name: string;
  kind: FeedbackKind;
  expectedTitle: string;
  attachElement: boolean;
  attachPage: boolean;
  action: AttachAction;
}

const VALID_ACTIONS = new Set<string>(['none', 'toggle_element', 'toggle_page', 'proceed', 'cancel', 'escape']);

function loadCases(): AttachCase[] {
  const path = resolve(process.cwd(), 'src/components/feedback/testdata/attach_options_cases.yaml');
  const root = requireRecord(parseStrictYAML(readFileSync(path, 'utf8'), 'attach options fixture'), 'attach options fixture');
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
    if (
      typeof row.expectedTitle !== 'string' ||
      typeof row.attachElement !== 'boolean' ||
      typeof row.attachPage !== 'boolean' ||
      typeof row.action !== 'string' ||
      !VALID_ACTIONS.has(row.action)
    ) {
      throw new Error(`cases[${index}] has invalid field types`);
    }
    return {
      name: row.name as string,
      kind,
      expectedTitle: row.expectedTitle,
      attachElement: row.attachElement,
      attachPage: row.attachPage,
      action: row.action as AttachAction,
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

    const elementCheckbox = screen.getByRole('checkbox', { name: 'pick an element on the page' }) as HTMLInputElement;
    const pageCheckbox = screen.getByRole('checkbox', { name: 'capture the visible page' }) as HTMLInputElement;
    expect(elementCheckbox.checked).toBe(c.attachElement);
    expect(pageCheckbox.checked).toBe(c.attachPage);
    expect(screen.getByText(/Captures mask transcript text by default/i)).toBeInTheDocument();

    if (c.action === 'toggle_element') {
      fireEvent.click(elementCheckbox);
      expect(onToggleElement).toHaveBeenCalledTimes(1);
      expect(onTogglePage).not.toHaveBeenCalled();
    } else if (c.action === 'toggle_page') {
      fireEvent.click(pageCheckbox);
      expect(onTogglePage).toHaveBeenCalledTimes(1);
      expect(onToggleElement).not.toHaveBeenCalled();
    } else if (c.action === 'proceed') {
      fireEvent.click(screen.getByRole('button', { name: 'continue' }));
      expect(onProceed).toHaveBeenCalledTimes(1);
    } else if (c.action === 'cancel') {
      fireEvent.click(screen.getByRole('button', { name: 'cancel' }));
      expect(onClose).toHaveBeenCalledTimes(1);
    } else if (c.action === 'escape') {
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
