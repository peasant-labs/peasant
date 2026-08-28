import type {
  SessionDetailPayload,
  SessionScorecard,
  ToolCallDetail,
  TurnDetail,
} from "@/types/messages";
import { Harness, StopReason, ToolCallKind } from "@/types/messages";

/**
 * The SAME recorded session the canonical fairtrade demo renders
 * (`sess_demo_0001` — "Port the transcript canvas into the shared package").
 * This file is a faithful port of the demo mockup's `buildWire()`: the editorial
 * fixtures below are projected into the canonical wire `SessionDetailPayload`,
 * which the visual-harness route feeds straight into the shared
 * `@peasant-labs/fairtrade` `<TranscriptViewer>` composite — exactly the
 * payload village's real `/transcripts/[id]` page hands it, minus the REST fetch.
 * Rendering the SAME data the demo renders lets the side-by-side capture stitch a
 * true height-matched, same-data comparison of the canonical demo vs the
 * assembled village frontend.
 *
 * WIRE FAITHFULNESS (the reason tool outputs render): on the real wire a tool
 * call's `arguments` AND `result` are JSON-ENCODED STRINGS — so a plain-text
 * result like file contents or stdout is carried as `JSON.stringify(text)`. The
 * adapter's sole parse boundary `JSON.parse`s them back; a RAW (non-JSON) string
 * would parse to `undefined` and the output would silently vanish. Every builder
 * below therefore JSON-encodes args + result, matching the demo's `toolToWire`.
 */

/* ── editorial fixtures (verbatim from the demo mockup) ─────────────────────── */

/** A tool fixture in the demo's editorial shape (pre-wire). */
interface ToolFixture {
  id: string;
  kind: "read" | "grep" | "bash" | "edit" | "task";
  name: string;
  /** read: file path + excerpt. */
  path?: string;
  excerpt?: string;
  /** grep: pattern/scope/type + results. */
  pattern?: string;
  scope?: string;
  glob?: string;
  results?: string;
  /** bash: command (+ description) + stdout + exit + duration. */
  command?: string;
  description?: string;
  stdout?: string;
  exit?: number;
  duration?: string;
  /** edit: file path + the hunk + churn. */
  adds?: number;
  dels?: number;
  hunk?: { sign: "ctx" | "add" | "del"; a?: string; b?: string; t: string }[];
  /** task/subagent: agent + description + prompt + result. */
  agent?: string;
  status?: string;
  task?: string;
  owner?: string;
  promptBody?: string;
  result?: string;
}

/** A turn fixture in the demo's editorial shape (pre-wire). */
interface TurnFixture {
  id: number;
  role: "user" | "assistant";
  depth?: number;
  subagent?: string;
  final?: boolean;
  error?: boolean;
  tokens?: { in: number; out: number };
  thinking?: { words: number; text: string };
  body?: string;
  tools?: ToolFixture[];
  checkpoint?: { hash: string; msg: string; files: number; adds: number; dels: number };
}

const HARNESS = Harness.ClaudeCode;

const TURNS: TurnFixture[] = [
  {
    id: 0,
    role: "user",
    tokens: { in: 280, out: 0 },
    body:
      "Port the transcript canvas into the shared package. Start by reading the existing renderer before extracting it.",
  },
  {
    id: 1,
    role: "assistant",
    tokens: { in: 1840, out: 920 },
    thinking: {
      words: 84,
      text:
        "The renderer lives under web/src/components/session-detail/v2/canvas. I should read TurnRow.tsx first to understand how role glyphs + tool calls compose, then trace rendererFor to see the per-tool dispatch before I move anything into packages/browser.",
    },
    body:
      "Let me look at the current renderer before extracting it. I will read **TurnRow.tsx** and find where `rendererFor` is wired so the move preserves the per-tool dispatch.",
    tools: [
      {
        id: "t1a",
        kind: "read",
        name: "Read",
        path: "web/src/components/session-detail/v2/canvas/TurnRow.tsx",
        excerpt:
          'export function TurnRow({ turn, depth }: TurnRowProps) {\n  const Glyph = roleGlyph(turn.role)\n  const tokens = formatTokens(turn.usage)\n  return (\n    <div className="turn-row" data-depth={depth}>\n      <RoleGlyph as={Glyph} provider={turn.provider} />\n      <TurnHeader label={turn.label} time={turn.time} />\n      <Markdown source={turn.content} />\n      <ToolCallList calls={turn.toolCalls} />\n    </div>\n  )\n}',
      },
    ],
  },
  {
    id: 2,
    role: "assistant",
    tokens: { in: 2100, out: 640 },
    body: "Now locate every call site of `rendererFor` so the dispatch table comes across intact.",
    tools: [
      {
        id: "t2a",
        kind: "grep",
        name: "Grep",
        pattern: "rendererFor",
        scope: "web/src",
        glob: "*.ts",
        results:
          "web/src/canvas/rendererFor.ts:12   export function rendererFor(kind: ToolKind) {\nweb/src/canvas/ToolCallRow.tsx:31   const Renderer = rendererFor(call.kind)\nweb/src/canvas/index.ts:4          export { rendererFor } from \"./rendererFor\"\nweb/src/session-detail/v2/canvas/TurnRow.tsx:58  rendererFor(call.kind)",
      },
    ],
  },
  {
    id: 3,
    role: "user",
    tokens: { in: 90, out: 0 },
    body: "Looks right. Run a typecheck across the workspace before you start moving files.",
  },
  {
    id: 4,
    role: "assistant",
    error: true,
    tokens: { in: 2400, out: 1180 },
    body: "Running the workspace typecheck first. It surfaces a strict-mode error in the tasks helper.",
    tools: [
      {
        id: "t4a",
        kind: "bash",
        name: "Bash",
        command: "pnpm -r typecheck",
        description: "workspace-wide typescript check",
        stdout:
          "packages/browser typecheck$ tsc -p tsconfig.json --noEmit\nsrc/lib/tasks.ts(48,9): error TS2532: Object is possibly 'undefined'.\n   46 |   const groups = groupByTask(turns)\n   47 |   const first = groups[0]\n>  48 |   return first.turns[0].index\n      |          ^^^^^\nELIFECYCLE  Command failed with exit code 2.",
        duration: "4.1s",
        exit: 2,
      },
    ],
  },
  {
    id: 5,
    role: "assistant",
    tokens: { in: 2600, out: 1320 },
    thinking: {
      words: 41,
      text:
        "`groups[0]` is possibly undefined under noUncheckedIndexedAccess. Guard it and return null for the empty case so the caller can fall back to turn 0.",
    },
    body: "The index access is unguarded under strict mode. Adding a null-guard so the empty-task case falls through cleanly.",
    tools: [
      {
        id: "t5a",
        kind: "edit",
        name: "Edit",
        path: "packages/browser/src/lib/tasks.ts",
        adds: 3,
        dels: 1,
        hunk: [
          { sign: "ctx", a: "46", b: "46", t: "  const groups = groupByTask(turns)" },
          { sign: "del", a: "47", t: "  const first = groups[0]" },
          { sign: "add", b: "47", t: "  const first = groups[0]" },
          { sign: "add", b: "48", t: "  if (!first || first.turns.length === 0) return null" },
          { sign: "add", b: "49", t: "  return first.turns[0]!.index" },
          { sign: "del", a: "48", t: "  return first.turns[0].index" },
        ],
      },
    ],
  },
  {
    id: 6,
    role: "assistant",
    tokens: { in: 1900, out: 980 },
    body: "Verifying the fix: the workspace typechecks clean now, so I will commit the canvas port.",
    tools: [
      {
        id: "t6a",
        kind: "bash",
        name: "Bash",
        command: "pnpm -r typecheck",
        stdout:
          "packages/browser typecheck$ tsc -p tsconfig.json --noEmit\nweb typecheck$ tsc -p tsconfig.json --noEmit\n\n  3 packages checked, 0 errors",
        duration: "5.6s",
        exit: 0,
      },
    ],
    checkpoint: { hash: "9f3c1ad", msg: "feat(canvas): port TurnRow + tool renderers", files: 7, adds: 312, dels: 24 },
  },
  {
    id: 7,
    role: "assistant",
    depth: 1,
    subagent: "docs-writer",
    tokens: { in: 1700, out: 1140 },
    body: "Spawned a subagent to document the props/callback/capability contract for the extracted package.",
    tools: [
      {
        id: "t7a",
        kind: "task",
        name: "Task",
        agent: "docs-writer",
        status: "completed",
        task: "Document the props/callback/capability contract",
        owner: "main",
        promptBody:
          "Write the README contract section for `<SessionDetail>`: every prop, every host callback (onDownload, linkBuilder, renderTurnActions), and the capability flags that gate the action menu.",
        result: "Drafted README.md contract section (78 lines). Documented 14 props, 3 callbacks, 4 capability flags.",
      },
    ],
  },
  {
    id: 8,
    role: "assistant",
    final: true,
    tokens: { in: 980, out: 720 },
    body:
      "All packages typecheck and build. The canvas now lives in `packages/browser` and `web` consumes it through the shared export.",
    tools: [
      {
        id: "t8a",
        kind: "bash",
        name: "Bash",
        command: "pnpm -r build && pnpm -r typecheck",
        stdout:
          "packages/browser build$ tsup\n  ESM dist/index.mjs 41.2 KB\n  CJS dist/index.js  43.9 KB\nweb build$ vite build\n  ✓ built in 6.41s\n\n  3 packages built, 0 errors",
        duration: "12.0s",
        exit: 0,
      },
    ],
  },
];

/* ── wire builders (mirror the demo's toolToWire / turnToWire) ──────────────── */

/** `"4.1s"` → `4100` ms. */
function secs(d: string | undefined): number | undefined {
  const m = typeof d === "string" ? d.match(/([\d.]+)s/) : null;
  return m && m[1] != null ? Math.round(parseFloat(m[1]) * 1000) : undefined;
}

/** A demo tool fixture → canonical `ToolCallDetail` (JSON-encoded arguments + result). */
function toolToWire(tool: ToolFixture): ToolCallDetail {
  let args: Record<string, unknown> = {};
  let result = "";
  let toolKind: ToolCallDetail["toolKind"];
  let filePath: string | undefined;
  let exitCode: number | undefined;
  let isError = false;
  let durationMs: number | undefined;

  switch (tool.kind) {
    case "read":
      args = { file_path: tool.path, offset: 1, limit: 40 };
      result = JSON.stringify(tool.excerpt ?? "");
      toolKind = ToolCallKind.Read;
      filePath = tool.path;
      break;
    case "grep":
      args = { pattern: tool.pattern, path: tool.scope, type: tool.glob };
      result = JSON.stringify(tool.results ?? "");
      toolKind = ToolCallKind.Search;
      break;
    case "bash":
      args = { command: tool.command, ...(tool.description ? { description: tool.description } : {}) };
      result = JSON.stringify(tool.stdout ?? "");
      toolKind = ToolCallKind.Execute;
      exitCode = tool.exit;
      isError = tool.exit !== 0;
      durationMs = secs(tool.duration);
      break;
    case "edit": {
      // Reconstruct pre/post text from the editorial hunk so the wire carries REAL
      // edit content and the adapter's LCS diff runs on it (matching the demo).
      const hunk = tool.hunk ?? [];
      const oldText = hunk.filter((d) => d.sign === "ctx" || d.sign === "del").map((d) => d.t).join("\n");
      const newText = hunk.filter((d) => d.sign === "ctx" || d.sign === "add").map((d) => d.t).join("\n");
      args = { file_path: tool.path, old_string: oldText, new_string: newText };
      result = JSON.stringify("ok");
      toolKind = ToolCallKind.Edit;
      filePath = tool.path;
      break;
    }
    case "task":
      args = { subagent_type: tool.agent, status: tool.status, description: tool.task, owner: tool.owner, prompt: tool.promptBody };
      result = JSON.stringify(tool.result ?? "");
      toolKind = ToolCallKind.Other;
      break;
  }

  const wire: ToolCallDetail = { id: tool.id, name: tool.name, arguments: JSON.stringify(args), result };
  if (toolKind) wire.toolKind = toolKind;
  if (filePath) wire.filePath = filePath;
  if (exitCode != null) wire.exitCode = exitCode;
  if (isError) wire.isError = true;
  if (durationMs != null) wire.durationMs = durationMs;
  return wire;
}

/** map a turn id → a monotonic RFC3339 timestamp (drives commit anchoring). */
const TS_BASE = Date.parse("2026-06-17T09:12:00Z");
const turnTimestamp = (id: number) => new Date(TS_BASE + id * 60_000).toISOString();

/** A demo turn fixture → canonical `TurnDetail`. */
function turnToWire(t: TurnFixture): TurnDetail {
  // Fold tool-sibling thinking into the turn content as a <thinking>…</thinking>
  // block so the ADAPTER (not a VM overlay) extracts it back out — the demo's
  // render-when-present convention.
  const content = t.thinking ? `<thinking>${t.thinking.text}</thinking>\n${t.body ?? ""}` : t.body ?? "";
  const wire: TurnDetail = {
    index: t.id,
    role: t.role,
    content,
    timestamp: turnTimestamp(t.id),
    depth: t.depth ?? 0,
  };
  if (t.thinking) wire.hasThinking = true;
  if (t.tools) wire.toolCalls = t.tools.map(toolToWire);
  if (t.subagent) wire.agentName = t.subagent;
  if (t.final) wire.stopReason = StopReason.EndTurn;
  if (t.tokens) {
    wire.tokensIn = t.tokens.in;
    wire.tokensOut = t.tokens.out;
  }
  return wire;
}

/**
 * Optional scorecard fixture — the per-session quality signals. Attaching it to
 * the payload lets the adapter's `assessScorecard` DERIVE the three verdict bands
 * (token / prompt / loop efficiency) the highlights-tab Scorecard renders. Remove
 * it and the card simply does not appear; the viewer still works.
 */
export const sampleScorecard: SessionScorecard = {
  totalTokens: 18420,
  retryTokensWasted: 1300,
  m5ContextUtilizationPct: 42,
  m6OutputSurvivalPct: 88,
  specQualityScore: 72,
  signalDensity: 64,
  m7SpecHasExamples: true,
  m7SpecHasConstraints: false,
  m4ConsecutiveErrorMax: 1,
  withinSessionReverts: 1,
  costTotalUsd: 0.42,
  outcome: "resolved",
};

/**
 * The canonical wire payload with flat git fields. The visual harness exercises
 * the same shape produced by the local API rather than relying on the retired
 * nested git-context compatibility input.
 */
export const sampleSession: SessionDetailPayload = {
  id: "sess_demo_0001",
  harness: HARNESS,
  startTime: turnTimestamp(0),
  endTime: turnTimestamp(8),
  durationMins: 8,
  totalTokens: 18420,
  tokensIn: 12200,
  tokensOut: 6200,
  turnCount: 8,
  toolCallCount: 5,
  project: "transcript-canvas",
  model: "claude-opus-4-7",
  workingDirectory: "/Users/dev/transcript-canvas",
  gitBranch: "lift/transcript-canvas",
  gitRemote: "https://example.com/peasant-labs/transcript-canvas.git",
  outcome: "resolved",
  turns: TURNS.map(turnToWire),
  scorecard: sampleScorecard,
};
