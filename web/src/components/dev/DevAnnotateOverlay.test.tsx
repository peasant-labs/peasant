import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import {
  parseStrictYAML,
  requireExactFields,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';
import {
  DevAnnotateOverlay,
  feedbackDisabled,
  feedbackLabelFor,
  feedbackPayloadFor,
  feedbackSelectorFor,
  feedbackTargetFrom,
} from './DevAnnotateOverlay';

// The dev inspect + feedback tool is mounted on the real app shell, so these cases
// drive the production component: arm the picker, hover, pick, write, save, re-arm,
// and back out with esc. The element-description and switch matrices live in the
// fixture below (see the workspace rule that keeps case tables out of test code).

const FIXTURE_PATH = resolve(process.cwd(), 'src/components/dev/testdata/inspect_feedback_targets.yaml');

type ElementCase = {
  name: string;
  html: string;
  pick: string;
  selector: string;
  label: string;
  section: string;
  anchor: string;
  snippetChars: number;
  padTo?: number;
};
type SwitchCase = { name: string; query: string; disabled: boolean };
type PayloadCase = {
  name: string;
  html: string;
  pick: string;
  route: string;
  comment: string;
  selector: string;
  anchor: string;
  commentStored: string;
  snippetChars: number;
};

function loadInventory(root: Record<string, unknown>, key: string): Record<string, unknown>[] {
  const inventory = requireRecord(root[key], `inspect feedback fixture.${key}`);
  requireExactRequiredFields(inventory, ['requiredNames', 'cases'], `inspect feedback fixture.${key}`);
  const requiredNames = inventory.requiredNames;
  if (
    !Array.isArray(requiredNames) ||
    requiredNames.length === 0 ||
    requiredNames.some((name) => typeof name !== 'string' || name.length === 0) ||
    new Set(requiredNames).size !== requiredNames.length
  ) {
    throw new Error(`inspect feedback fixture.${key}.requiredNames must be unique non-empty strings`);
  }
  if (!Array.isArray(inventory.cases) || inventory.cases.length === 0) {
    throw new Error(`inspect feedback fixture.${key}.cases must be a non-empty array`);
  }
  const cases = inventory.cases.map((row, index) => requireRecord(row, `inspect feedback fixture.${key}.cases[${index}]`));
  requireUniqueNames(cases, `inspect feedback fixture.${key}.cases`);
  const names = cases.map((row) => row.name as string);
  const missing = (requiredNames as string[]).filter((name) => !names.includes(name));
  const undeclared = names.filter((name) => !(requiredNames as string[]).includes(name));
  if (missing.length > 0 || undeclared.length > 0) {
    throw new Error(
      `inspect feedback fixture.${key} cases must match requiredNames exactly (missing: ${missing.join(', ') || 'none'}; undeclared: ${undeclared.join(', ') || 'none'})`,
    );
  }
  return cases;
}

function loadFixture() {
  const root = requireRecord(parseStrictYAML(readFileSync(FIXTURE_PATH, 'utf8'), 'inspect feedback fixture'), 'inspect feedback fixture');
  requireExactRequiredFields(root, ['elements', 'switches', 'payloads', 'designSystemClasses'], 'inspect feedback fixture');

  const elementRows = loadInventory(root, 'elements');
  elementRows.forEach((row, index) => {
    requireExactFields(row, ['name', 'html', 'pick', 'selector', 'label', 'section', 'anchor', 'snippetChars', 'padTo'], `elements[${index}]`);
    const strings = ['html', 'pick', 'selector', 'label', 'section', 'anchor'] as const;
    if (strings.some((field) => typeof row[field] !== 'string') || !Number.isSafeInteger(row.snippetChars)) {
      throw new Error(`elements[${index}] has invalid field types`);
    }
    if (row.padTo !== undefined && (!Number.isSafeInteger(row.padTo) || (row.padTo as number) <= 0)) {
      throw new Error(`elements[${index}].padTo must be a positive integer when present`);
    }
  });

  const switchRows = loadInventory(root, 'switches');
  switchRows.forEach((row, index) => {
    requireExactRequiredFields(row, ['name', 'query', 'disabled'], `switches[${index}]`);
    if (typeof row.query !== 'string' || typeof row.disabled !== 'boolean') {
      throw new Error(`switches[${index}] has invalid field types`);
    }
  });

  const payloadRows = loadInventory(root, 'payloads');
  payloadRows.forEach((row, index) => {
    requireExactRequiredFields(row, ['name', 'html', 'pick', 'route', 'comment', 'selector', 'anchor', 'commentStored', 'snippetChars'], `payloads[${index}]`);
    const strings = ['html', 'pick', 'route', 'comment', 'selector', 'anchor', 'commentStored'] as const;
    if (strings.some((field) => typeof row[field] !== 'string') || !Number.isSafeInteger(row.snippetChars)) {
      throw new Error(`payloads[${index}] has invalid field types`);
    }
  });

  const designSystem = requireRecord(root.designSystemClasses, 'inspect feedback fixture.designSystemClasses');
  requireExactRequiredFields(designSystem, ['requiredNames'], 'inspect feedback fixture.designSystemClasses');
  const classes = designSystem.requiredNames;
  if (!Array.isArray(classes) || classes.length === 0 || classes.some((name) => typeof name !== 'string' || !/^[a-z0-9-]+$/.test(name))) {
    throw new Error('inspect feedback fixture.designSystemClasses.requiredNames must be class names');
  }

  return {
    elements: elementRows as unknown as ElementCase[],
    switches: switchRows as unknown as SwitchCase[],
    payloads: payloadRows as unknown as PayloadCase[],
    designSystemClasses: classes as string[],
  };
}

const fixture = loadFixture();

/** Mounts a case's markup as the page body and returns the picked element. */
function mountCase(c: { html: string; pick: string; padTo?: number }): Element {
  const html = c.padTo ? c.html.replace('PAD', 'x'.repeat(c.padTo)) : c.html;
  document.body.innerHTML = html;
  const el = document.body.querySelector(c.pick);
  if (!el) throw new Error(`case markup does not match ${c.pick}`);
  return el;
}

describe('inspect + feedback element descriptions', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it.each(fixture.elements)('$name', (c) => {
    const el = mountCase(c);
    const target = feedbackTargetFrom(el);
    expect(target.selector).toBe(c.selector);
    expect(target.label).toBe(c.label);
    expect(target.section).toBe(c.section);
    expect(target.snippet.length).toBe(c.snippetChars);
    expect(target.snippet).toBe(el.outerHTML.slice(0, c.snippetChars));
    expect(feedbackPayloadFor(el, '/', '').anchor).toBe(c.anchor);
  });
});

describe('inspect + feedback disable switch', () => {
  it.each(fixture.switches)('$name', (c) => {
    expect(feedbackDisabled(c.query)).toBe(c.disabled);
  });
});

describe('inspect + feedback payload', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it.each(fixture.payloads)('$name', (c) => {
    const el = mountCase(c);
    expect(feedbackPayloadFor(el, c.route, c.comment)).toEqual({
      route: c.route,
      anchor: c.anchor,
      selector: c.selector,
      snippet: el.outerHTML.slice(0, c.snippetChars),
      comment: c.commentStored,
    });
  });
});

describe('inspect + feedback design-system chrome', () => {
  it('renders classes the published design system still ships', () => {
    const cssPath = resolve(process.cwd(), 'node_modules/@peasant-labs/fairtrade/dist/lib/components.css');
    const css = readFileSync(cssPath, 'utf8');
    for (const cls of fixture.designSystemClasses) {
      expect(
        css,
        `${cls} is missing from @peasant-labs/fairtrade components.css — re-pin fairtrade or restore the class, or the tool renders unstyled`,
      ).toContain(`.${cls}`);
    }
  });
});

describe('DevAnnotateOverlay mounted flow', () => {
  const fetchMock = vi.fn();
  vi.stubGlobal('fetch', fetchMock);

  beforeEach(() => {
    fetchMock.mockReset();
    fetchMock.mockResolvedValue(Response.json({ ok: true, path: '/repo/llm/ui-feedback.md' }));
    window.history.replaceState({}, '', '/');
  });

  afterEach(() => {
    cleanup();
    document.body.innerHTML = '';
    window.history.replaceState({}, '', '/');
  });

  const launch = () => screen.getByRole('button', { name: 'comment on an element (c)' });
  const selecting = () => screen.getByRole('button', { name: 'stop selecting an element to comment on (esc)' });
  const noteBox = () => screen.queryByPlaceholderText('what should change here?');
  const hoverTag = () => document.querySelector('.fbk-tag') as HTMLElement | null;

  function renderTool() {
    return render(<DevAnnotateOverlay />);
  }

  /** Picks `el` through the armed picker; returns the focused note textarea. */
  async function pick(el: Element) {
    fireEvent.mouseMove(el, { clientX: 20, clientY: 20 });
    fireEvent.click(el);
    const textarea = await screen.findByPlaceholderText('what should change here?');
    await waitFor(() => expect(document.body.classList.contains('fbk-arming')).toBe(false));
    return textarea;
  }

  /** Waits for a save to land and the picker to re-arm for the next element. */
  async function waitForRearm() {
    await waitFor(() => expect(noteBox()).toBeNull());
    await waitFor(() => expect(selecting()).toHaveAttribute('aria-pressed', 'true'));
  }

  function appendElement(tag: string, className: string, text = 'x'): Element {
    const el = document.createElement(tag);
    el.className = className;
    el.textContent = text;
    document.body.appendChild(el);
    return el;
  }

  it('arms and disarms from the comment control', () => {
    renderTool();
    fireEvent.click(launch());
    expect(selecting()).toHaveAttribute('aria-pressed', 'true');
    expect(document.body.classList.contains('fbk-arming')).toBe(true);
    fireEvent.click(selecting());
    expect(launch()).toHaveAttribute('aria-pressed', 'false');
    expect(document.body.classList.contains('fbk-arming')).toBe(false);
  });

  it('arms from the c shortcut and ignores the shortcut while typing', () => {
    render(<><DevAnnotateOverlay /><input aria-label="note field" /></>);
    fireEvent.keyDown(window, { key: 'c' });
    expect(selecting()).toHaveAttribute('aria-pressed', 'true');
    fireEvent.keyDown(window, { key: 'Escape' });
    expect(launch()).toHaveAttribute('aria-pressed', 'false');

    const input = screen.getByLabelText('note field');
    input.focus();
    fireEvent.keyDown(input, { key: 'c' });
    expect(launch()).toHaveAttribute('aria-pressed', 'false');
    expect(document.body.classList.contains('fbk-arming')).toBe(false);
  });

  it('outlines the hovered element and shows a cheap tag without its text', async () => {
    renderTool();
    fireEvent.click(launch());
    const section = document.createElement('section');
    section.setAttribute('aria-label', 'Files changed');
    const heavy = document.createElement('div');
    heavy.className = 'big panel extra';
    heavy.textContent = 'transcript body '.repeat(500);
    section.appendChild(heavy);
    document.body.appendChild(section);

    fireEvent.mouseMove(heavy, { clientX: 30, clientY: 40 });
    await waitFor(() => expect(heavy.classList.contains('fbk-target')).toBe(true));
    await waitFor(() => expect(hoverTag()?.style.display).toBe('block'));
    expect(hoverTag()?.textContent).toBe('Files changed / div.big.panel.extra');
    expect(hoverTag()?.textContent).not.toContain('transcript body');

    fireEvent.scroll(window);
    expect(heavy.classList.contains('fbk-target')).toBe(false);
    expect(hoverTag()?.style.display).toBe('none');
  });

  it('skips its own chrome while the picker is armed', () => {
    renderTool();
    const button = launch();
    fireEvent.click(button);
    fireEvent.mouseMove(button, { clientX: 5, clientY: 5 });
    expect(button.classList.contains('fbk-target')).toBe(false);
    expect(hoverTag()?.style.display).not.toBe('block');
  });

  it('opens a focused note on the picked element with its target description', async () => {
    renderTool();
    fireEvent.click(launch());
    const section = document.createElement('section');
    section.setAttribute('aria-label', 'Sessions');
    const el = document.createElement('div');
    el.className = 'row wide';
    section.appendChild(el);
    document.body.appendChild(section);

    const textarea = await pick(el);
    await waitFor(() => expect(document.activeElement).toBe(textarea));
    expect(document.querySelector('.fbk-pop-tgt')?.textContent).toBe(`Sessions  ${feedbackSelectorFor(el)}`);
    expect(screen.getByRole('button', { name: 'cancel (esc)' })).toBeInTheDocument();
  });

  it('saves with cmd/ctrl+Enter, posts the note, and re-arms for the next pick', async () => {
    renderTool();
    fireEvent.click(launch());
    const el = appendElement('button', 'row wide', 'open');
    const textarea = await pick(el);
    fireEvent.change(textarea, { target: { value: '  the row wraps  ' } });
    fireEvent.keyDown(textarea, { key: 'Enter', metaKey: true });

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/local/feedback');
    const init = fetchMock.mock.calls[0][1] as RequestInit;
    expect(init.method).toBe('POST');
    expect(JSON.parse(String(init.body))).toEqual({
      route: window.location.pathname,
      anchor: feedbackLabelFor(el),
      selector: feedbackSelectorFor(el),
      snippet: el.outerHTML,
      comment: 'the row wraps',
    });
    expect(await screen.findByText('saved to llm/ui-feedback.md')).toBeInTheDocument();

    // The picker re-armed: the note is gone and the next element is one click away.
    await waitForRearm();
    expect(document.body.classList.contains('fbk-arming')).toBe(true);

    // A second note through the popup's save button, a third through ctrl+Enter.
    const second = appendElement('span', 'chip', 'second');
    const secondBox = await pick(second);
    fireEvent.change(secondBox, { target: { value: 'second note' } });
    fireEvent.click(screen.getByRole('button', { name: /save/ }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    await waitForRearm();

    const third = appendElement('em', 'marker', 'third');
    const thirdBox = await pick(third);
    fireEvent.change(thirdBox, { target: { value: 'third note' } });
    fireEvent.keyDown(thirdBox, { key: 'Enter', ctrlKey: true });
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
    expect(JSON.parse(String((fetchMock.mock.calls[2][1] as RequestInit).body)).comment).toBe('third note');
  });

  it('backs out one step at a time with esc: cancel the note, then disarm', async () => {
    renderTool();
    fireEvent.click(launch());
    const el = appendElement('div', 'row');
    const textarea = await pick(el);
    fireEvent.change(textarea, { target: { value: 'a draft I do not want' } });

    fireEvent.keyDown(textarea, { key: 'Escape' });
    await waitFor(() => expect(noteBox()).toBeNull());
    expect(selecting()).toHaveAttribute('aria-pressed', 'true');
    expect(document.body.classList.contains('fbk-arming')).toBe(true);
    expect(fetchMock).not.toHaveBeenCalled();

    fireEvent.keyDown(window, { key: 'Escape' });
    expect(launch()).toHaveAttribute('aria-pressed', 'false');
    expect(document.body.classList.contains('fbk-arming')).toBe(false);
  });

  it('cancels from the popup without posting', async () => {
    renderTool();
    fireEvent.click(launch());
    const textarea = await pick(appendElement('div', 'row'));
    fireEvent.change(textarea, { target: { value: 'never mind' } });
    fireEvent.click(screen.getByRole('button', { name: 'cancel (esc)' }));
    await waitFor(() => expect(noteBox()).toBeNull());
    expect(selecting()).toHaveAttribute('aria-pressed', 'true');
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('does not post a blank note and keeps the note box focused', async () => {
    renderTool();
    fireEvent.click(launch());
    const textarea = await pick(appendElement('div', 'row'));
    fireEvent.change(textarea, { target: { value: '   ' } });
    fireEvent.keyDown(textarea, { key: 'Enter', metaKey: true });
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(fetchMock).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(textarea);
  });

  it('keeps the note and reports the error when the save fails', async () => {
    fetchMock.mockResolvedValueOnce(new Response('nope', { status: 500, statusText: 'Internal Server Error' }));
    renderTool();
    fireEvent.click(launch());
    const textarea = await pick(appendElement('div', 'row'));
    fireEvent.change(textarea, { target: { value: 'keep me' } });
    fireEvent.keyDown(textarea, { key: 'Enter', metaKey: true });

    expect(await screen.findByText('error: 500 Internal Server Error')).toBeInTheDocument();
    expect(noteBox()).toHaveValue('keep me');
    expect(screen.queryByRole('button', { name: /stop selecting/ })).toBeNull();
  });

  it('stays off for the whole page with ?fb=off', () => {
    window.history.replaceState({}, '', '/?fb=off');
    renderTool();
    expect(screen.queryByRole('button', { name: 'comment on an element (c)' })).toBeNull();
    expect(document.querySelector('[data-fb]')).toBeNull();
    fireEvent.keyDown(window, { key: 'c' });
    expect(document.body.classList.contains('fbk-arming')).toBe(false);
  });
});
