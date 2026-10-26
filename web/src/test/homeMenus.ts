import { act, fireEvent, screen, within } from '@testing-library/react';

/**
 * Home's two radio menus, driven the way a person drives them: open the menu
 * from its trigger, then pick a row. The rows of a closed menu are hidden, so
 * a test that only reads them passes `{ hidden: true }` through `choiceRow`.
 */

const BROWSE_TRIGGER = /^browse by:/;
const FILTERS_TRIGGER = /^filters/;

function menuOf(trigger: HTMLElement): HTMLElement {
  const id = trigger.getAttribute('aria-controls');
  const pop = id ? document.getElementById(id) : null;
  if (!pop) throw new Error(`menu trigger "${trigger.textContent}" controls no menu`);
  return pop;
}

async function pick(trigger: HTMLElement, name: string | RegExp): Promise<void> {
  if (trigger.getAttribute('aria-expanded') !== 'true') await act(async () => { fireEvent.click(trigger); });
  const row = within(menuOf(trigger)).getByRole('menuitemradio', { name });
  await act(async () => { fireEvent.click(row); });
}

/** The `browse by` trigger, named by the view it shows. */
export function browseTrigger(): HTMLElement {
  return screen.getByRole('button', { name: BROWSE_TRIGGER });
}

/** Switches Home to one of its views: `sessions`, `project`, `helper threads`, `every session`. */
export async function browseBy(view: string | RegExp): Promise<void> {
  await pick(await screen.findByRole('button', { name: BROWSE_TRIGGER }), view);
}

/** The `filters` trigger; its name counts the filters that are set. */
export function filtersTrigger(): HTMLElement {
  return screen.getByRole('button', { name: FILTERS_TRIGGER });
}

/** Picks a publish-state filter by its label. */
export async function chooseStatus(label: string): Promise<void> {
  await pick(filtersTrigger(), label);
}

/** Picks the project the list is scoped to, by its name (`all projects` clears it). */
export async function chooseProject(name: string): Promise<void> {
  await pick(filtersTrigger(), name);
}

/** The filter rows a group offers, open or not: `status` or `project`. */
export function choiceRows(group: 'status' | 'project'): HTMLElement[] {
  const menu = menuOf(filtersTrigger());
  return within(within(menu).getByRole('group', { name: group, hidden: true })).queryAllByRole('menuitemradio', { hidden: true });
}

/** One project row in the filters menu, open or not, or null. */
export function projectChoice(name: string): HTMLElement | null {
  return choiceRows('project').find((row) => row.textContent?.trim() === name) ?? null;
}
