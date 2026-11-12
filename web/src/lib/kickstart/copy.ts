/**
 * The words a kickstart page says about its own session: opening it, its end,
 * a save the server refused, and the rows that need a choice. They depend on
 * the scope and on nothing else.
 *
 * `kickstartCopy` is the one place that maps a scope to its words. It switches
 * over the closed `KickstartScope` set with no fallback, so a scope added to
 * the set stops the build here until it has its own copy; no page names a
 * scope to pick a sentence. What the server says (stop titles, hints, field
 * labels, summaries, errors) is never in this table: it is drawn as served.
 *
 * Chrome is lowercase, as the design system sets it.
 */

import { LocalKickstartErrorCode } from '@peasant-labs/schema';
import { KickstartScope } from './values';

export interface KickstartCopy {
  /** The label of the placeholder drawn while the session opens. */
  readonly opening: string;
  /** The title of the panel drawn when the session could not open. */
  readonly openFailed: string;
  /** The notice after the server refused a save and named `count` rows. */
  refused(count: number): string;
  /** The panel drawn when the session timed out. */
  readonly expired: {
    readonly title: string;
    readonly body: string;
    /** The button that opens a new session. */
    readonly reopen: string;
  };
  /** The heading and help of the rows whose saved value moved while the page was open. */
  readonly conflicts: {
    readonly title: string;
    readonly help: string;
  };
  /** The heading of the panel drawn after a save. */
  readonly saved: string;
}

const WIZARD: KickstartCopy = Object.freeze({
  opening: 'opening setup',
  openFailed: 'setup could not start',
  refused: (count: number) =>
    `nothing was saved: ${count === 1 ? 'a value' : 'some values'} changed in your saved settings while you set up. settle ${count === 1 ? 'it' : 'them'} on this page, then save again.`,
  expired: Object.freeze({
    title: 'this setup session timed out',
    body: 'nothing was saved, and the answers you had not saved are gone. open a new one to start again from your saved settings.',
    reopen: 'open a new setup',
  }),
  conflicts: Object.freeze({
    title: 'saved values that changed while you set up',
    help: 'these were changed in your saved settings after you answered them here. choose which value to keep for each one.',
  }),
  saved: 'your setup is saved',
});

const SELECTION: KickstartCopy = Object.freeze({
  opening: 'opening the selection',
  openFailed: 'the selection could not be opened',
  refused: (count: number) =>
    `nothing was saved: ${count === 1 ? 'a value' : 'some values'} changed in your saved settings while you chose sessions. settle ${count === 1 ? 'it' : 'them'} on this page, then save again.`,
  expired: Object.freeze({
    title: 'this selection session timed out',
    body: 'nothing was saved, and the choices you had not saved are gone. reopen it to start again from your saved selection.',
    reopen: 'reopen the selection',
  }),
  conflicts: Object.freeze({
    title: 'saved values that changed while you chose sessions',
    help: 'these were changed in your saved settings after you chose them here. choose which value to keep for each one.',
  }),
  saved: 'your selection is saved',
});

function unreachableScope(scope: never): never {
  throw new TypeError(
    `kickstartCopy has no words for the scope ${JSON.stringify(scope)}. ` +
    'this happened while a page chose its copy, so nothing could be drawn. ' +
    'add the scope to kickstartCopy in web/src/lib/kickstart/copy.ts.',
  );
}

/** The words a page of this scope uses. Exhaustive: a new scope is a compile error here. */
export function kickstartCopy(scope: KickstartScope): KickstartCopy {
  switch (scope.wire) {
    case KickstartScope.Full.wire:
      return WIZARD;
    case KickstartScope.Selection.wire:
      return SELECTION;
    default:
      return unreachableScope(scope);
  }
}

/**
 * What the page adds after the server's own sentence for a refusal, only where
 * the page knows something the sentence cannot: what this page does by itself.
 * Every other refusal is shown as the server wrote it, with nothing added. The
 * keys are the contract's closed refusal codes, never strings.
 */
const REFUSAL_ADVICE: ReadonlyMap<LocalKickstartErrorCode, string> = new Map([
  [
    LocalKickstartErrorCode.SessionCommitting,
    'you do not need to reload: this page moves to the finish by itself when the save ends.',
  ],
  [
    LocalKickstartErrorCode.FieldHidden,
    'another answer hides that question now. the page shows the questions that apply, so answer again there if it still asks.',
  ],
]);

/** The sentence to add after the server's own for this refusal, or null when the server's says it all. */
export function refusalAdvice(code: LocalKickstartErrorCode): string | null {
  return REFUSAL_ADVICE.get(code) ?? null;
}
