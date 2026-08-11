/**
 * The lay glossary — one plain-language definition per domain word, defined
 * once and rendered via <Term k="…"> (web/src/components/Term.tsx).
 *
 * Rule: lay term first, the engineering name in parentheses once. Explain by
 * consequence ("click to read the conversation behind it"), never by circular
 * definition. Honest verbs only — never "proves", "guarantees", "quality".
 */

export interface GlossaryEntry {
  /** The lay term shown by default (children can override the visible text). */
  term: string;
  /** The tooltip definition — one or two short sentences, plain words. */
  short: string;
}

export const GLOSSARY = {
  // --- the changes / git layer ---------------------------------------------
  change: {
    term: 'line of work',
    short:
      "A separate line of work split off from the project's main version (engineers call it a branch). It's the local version of what becomes a pull request (PR).",
  },
  defaultBranch: {
    term: "main line ('develop')",
    short:
      "The project's main version — the line everyone's work eventually folds back into. Here it's called 'develop'.",
  },
  commit: {
    term: 'saved update',
    short: 'A saved snapshot of the code at one moment. The squares on the timeline.',
  },
  merged: {
    term: 'folded into the main line',
    short: "This line of work was folded back into the project's main version — it's done.",
  },
  ahead: {
    term: 'updates not yet in the main line',
    short: "How many saved updates this work added that the main line doesn't have yet.",
  },

  // --- a line of work's freshness (open-branch lifecycle) -------------------
  active: {
    term: 'worked on recently',
    short: 'This line of work was touched in the last 3 days — someone is on it now.',
  },
  idle: {
    term: 'paused',
    short: 'No work on this line for 3 to 14 days — set aside for now, not abandoned.',
  },
  stale: {
    term: 'untouched for weeks',
    short: 'No work on this line for over two weeks — likely forgotten, or finished without folding in.',
  },
  pr: {
    term: 'pull request (PR)',
    short:
      "The request to fold a line of work back into the main version, where teammates review it. This is its local, pre-share form.",
  },
  repo: {
    term: 'version history',
    short:
      "The saved change history of a project (a git repository). Without it there are no 'changes' to compare.",
  },
  diff: {
    term: 'line-by-line edits',
    short: 'Every individual line this work added or removed across the files it touched.',
  },

  // --- the recorded conversation layer -------------------------------------
  session: {
    term: 'AI conversation',
    short:
      'One full conversation with a coding agent, captured end to end. Click to read it.',
  },
  task: {
    term: 'request',
    short:
      'One ask inside a conversation — your message and everything the agent did until your next message.',
  },
  recorded: {
    term: 'recorded',
    short:
      'Built while peasant was capturing the AI conversation — so the "why" behind the code is replayable, not guessed.',
  },
  transcript: {
    term: 'transcript',
    short: 'The full saved text of an AI conversation — every message and tool action.',
  },
  scope: {
    term: 'showing just this part',
    short:
      'The slice of the conversation in view — the whole thing, one request, or only what touched a given file or change. Clear it to see everything.',
  },
  coverage: {
    term: 'AI-built files',
    short:
      'How much of this code was last edited during a recorded AI conversation. The rest predates recording, or was edited outside this tool.',
  },

  // --- the map / structure layer -------------------------------------------
  node: {
    term: 'code area',
    short: 'A box on the map: a folder, package, or file, depending on how far you zoom in.',
  },
  structureEdge: {
    term: 'connection',
    short: 'A line between two code areas — one area uses (imports) the other.',
  },
  violation: {
    term: 'rule break',
    short:
      "Connections that tangle: two areas each use the other, or one points against the project's layering. The only red on the map.",
  },
  darkMatter: {
    term: 'no conversation behind it',
    short: 'Code with no recorded AI conversation behind it. Still real — just storyless.',
  },
  slice: {
    term: 'the touched part of the map',
    short: 'The piece of the code map this work rewired — what got changed and reconnected.',
  },
  timeStrip: {
    term: 'activity over time',
    short: 'Each bar is one day; taller means more AI conversations happened that day.',
  },
  shapedBy: {
    term: 'conversations that built this',
    short: 'The recorded AI conversations whose edits last touched this code area.',
  },
  oftenEditedWith: {
    term: 'usually changed alongside',
    short: 'Code areas that tend to get edited in the same conversations as this one.',
  },

  // --- binding / attribution -----------------------------------------------
  linked: {
    term: 'linked',
    short:
      "Both signals match: this conversation's saved updates AND its edited files fall inside this work.",
  },
  candidate: {
    term: 'possibly related',
    short:
      'Only one signal matched — it might belong to this work, shown so nothing is hidden. Could be unrelated.',
  },
  unrecorded: {
    term: 'updates without a saved conversation',
    short: 'Saved updates with no AI conversation captured — likely written by hand, or recorded outside this tool.',
  },

  // --- metrics -------------------------------------------------------------
  outputTokens: {
    term: 'AI text generated',
    short: 'Roughly how much text the agents produced doing this work. A token is about ¾ of a word.',
  },
  cost: {
    term: 'estimated AI spend',
    short: 'A rough estimate of what this work cost in AI usage, from token counts and model prices.',
  },

  // --- product surfaces / chrome -------------------------------------------
  connection: {
    term: 'Live · local',
    short: 'Connected to the Peasant app running on this computer — no internet involved.',
  },
  contribute: {
    term: 'Contribute',
    short:
      "Choose AI conversations to share with your team's shared library. Nothing is sent until you pick and confirm.",
  },
} as const satisfies Record<string, GlossaryEntry>;

export type GlossaryKey = keyof typeof GLOSSARY;

/** Look up a glossary entry; throws in dev if the key is unknown (caught by tests). */
export function getTerm(key: GlossaryKey): GlossaryEntry {
  return GLOSSARY[key];
}
