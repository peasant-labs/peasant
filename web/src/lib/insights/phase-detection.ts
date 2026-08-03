/**
 * Rule-based transcript phase detection.
 *
 * Segments transcript turns into phases, then merges micro-phases to
 * reduce fragmentation. Target: 4–8 merged phases for a 50-turn session.
 */

import type { TurnDetail } from "@/types/messages";
import type { Phase, PhaseType } from "./types";

interface RetryLoop {
  startTurn: number;
  endTurn: number;
  attemptCount: number;
  tools: string[];
}

// ---------------------------------------------------------------------------
// Tool classification helpers
// ---------------------------------------------------------------------------

const READ_TOOLS = new Set(["Read", "Glob", "Grep", "WebSearch", "WebFetch"]);
const WRITE_TOOLS = new Set(["Write", "Edit", "NotebookEdit"]);

const TEST_PATTERNS = [
  /\btest\b/i, /\bjest\b/i, /\bpytest\b/i, /\bgo\s+test\b/i,
  /\bnpm\s+test\b/i, /\bmake\s+check\b/i, /\bcargo\s+test\b/i,
  /\bvitest\b/i, /\bplaywright\b/i, /\bmocha\b/i, /\brspec\b/i,
];

function hasTools(turn: TurnDetail, toolSet: Set<string>): boolean {
  return turn.toolCalls?.some((tc) => toolSet.has(tc.name)) ?? false;
}

function hasOnlyTools(turn: TurnDetail, toolSet: Set<string>): boolean {
  if (!turn.toolCalls?.length) return false;
  return turn.toolCalls.every((tc) => toolSet.has(tc.name));
}

function hasError(turn: TurnDetail): boolean {
  if (turn.toolCalls) {
    for (const tc of turn.toolCalls) {
      if (tc.isError) return true;
      if (tc.exitCode !== undefined && tc.exitCode !== 0) return true;
    }
  }
  return false;
}

function isTesting(turn: TurnDetail): boolean {
  if (!turn.toolCalls) return false;
  for (const tc of turn.toolCalls) {
    if (tc.name !== "Bash") continue;
    const args = tc.arguments;
    if (TEST_PATTERNS.some((p) => p.test(args))) return true;
  }
  return false;
}

// ---------------------------------------------------------------------------
// Retry-loop detection across multi-tool sequences
// ---------------------------------------------------------------------------

interface Attempt {
  tools: string[];
  turnRange: [number, number];
  exitCode: number;
}

function extractFailedAttempts(turns: TurnDetail[]): Attempt[] {
  const attempts: Attempt[] = [];
  let currentTools: string[] = [];
  let startIdx = -1;

  for (let i = 0; i < turns.length; i++) {
    const turn = turns[i];
    if (turn.role === "user") {
      if (currentTools.length > 0 && startIdx >= 0) {
        currentTools = [];
        startIdx = -1;
      }
      continue;
    }

    if (!turn.toolCalls?.length) continue;

    if (startIdx < 0) startIdx = i;

    for (const tc of turn.toolCalls) {
      currentTools.push(tc.name);

      const failed =
        tc.isError ||
        (tc.exitCode !== undefined && tc.exitCode !== 0);

      if (failed && currentTools.length > 0) {
        const hasWrite = currentTools.some(
          (t) => WRITE_TOOLS.has(t) || t === "Bash",
        );
        if (hasWrite) {
          attempts.push({
            tools: [...currentTools],
            turnRange: [startIdx, i],
            exitCode: tc.exitCode ?? 1,
          });
        }
        currentTools = [];
        startIdx = -1;
      }
    }
  }

  return attempts;
}

function toolOverlap(a: string[], b: string[]): number {
  const setA = new Set(a);
  const setB = new Set(b);
  const intersection = [...setA].filter((t) => setB.has(t)).length;
  const union = new Set([...setA, ...setB]).size;
  return union > 0 ? intersection / union : 0;
}

function detectRetryLoops(turns: TurnDetail[]): RetryLoop[] {
  const attempts = extractFailedAttempts(turns);
  const loops: RetryLoop[] = [];
  let i = 0;

  while (i < attempts.length) {
    let j = i + 1;
    const groupTools = new Set(attempts[i].tools);

    // Group consecutive attempts with >50% tool name overlap
    while (j < attempts.length) {
      const overlap = toolOverlap(attempts[i].tools, attempts[j].tools);
      if (overlap < 0.5) break;
      for (const t of attempts[j].tools) groupTools.add(t);
      j++;
    }

    const attemptCount = j - i;
    if (attemptCount >= 2) {
      loops.push({
        startTurn: attempts[i].turnRange[0],
        endTurn: attempts[j - 1].turnRange[1],
        attemptCount,
        tools: [...groupTools],
      });
    }

    i = j;
  }

  return loops;
}

// ---------------------------------------------------------------------------
// Per-turn phase classification
// ---------------------------------------------------------------------------

function classifyTurn(
  turn: TurnDetail,
  displayIndex: number,
  prevPhase: PhaseType | null,
  recentErrorCount: number,
  retryRanges: [number, number][],
): PhaseType {
  // Check if turn is inside a retry loop range (ranges use display positions)
  for (const [start, end] of retryRanges) {
    if (displayIndex >= start && displayIndex <= end) return "retry-loop";
  }

  // User correction detection
  if (turn.role === "user") {
    const correctionSignals = [
      /\bi already said\b/i, /\bi told you\b/i, /\bno,?\s+not that\b/i,
      /\bthat'?s not what i\b/i, /\bnot what i asked\b/i,
      /\bwrong,?\s+i\b/i, /\bplease (re-?read|read again)\b/i,
    ];
    if (correctionSignals.some((p) => p.test(turn.content))) {
      return "user-correction";
    }
    // Treat other user turns as continuing the current phase
    return prevPhase ?? "planning";
  }

  // Error detection — check before testing so failed tests are "error" not "testing"
  if (hasError(turn)) return "error";

  // Testing detection (only for passing tests)
  if (isTesting(turn)) return "testing";

  // Debug: read tools after error
  if (recentErrorCount > 0 && hasOnlyTools(turn, READ_TOOLS)) return "debug";

  // Implementation: write tools
  if (hasTools(turn, WRITE_TOOLS)) {
    // Recovery if coming from retry loop or user correction
    if (prevPhase === "retry-loop" || prevPhase === "user-correction") {
      return "recovery";
    }
    return "implementation";
  }

  // Exploration: read-only tools not preceded by error
  if (hasOnlyTools(turn, READ_TOOLS) && recentErrorCount === 0) {
    return "exploration";
  }

  // Planning: assistant turn with no tool calls and substantial content
  if (turn.role === "assistant" && !turn.toolCalls?.length && turn.content.length > 100) {
    return "planning";
  }

  // Default: continue previous phase or mark as planning
  return prevPhase ?? "planning";
}

// ---------------------------------------------------------------------------
// Phase detection + merge
// ---------------------------------------------------------------------------

export function detectPhases(turns: TurnDetail[]): Phase[] {
  if (turns.length === 0) return [];

  // Pre-compute retry loop ranges
  const loops = detectRetryLoops(turns);
  const retryRanges: [number, number][] = loops.map((l) => [l.startTurn, l.endTurn]);

  // Classify each turn
  const rawPhases: { type: PhaseType; turn: number }[] = [];
  let recentErrors = 0;
  const errorWindow = 3;

  for (let i = 0; i < turns.length; i++) {
    // Update recent error count
    recentErrors = 0;
    for (let j = Math.max(0, i - errorWindow); j < i; j++) {
      if (hasError(turns[j])) recentErrors++;
    }

    const prevPhase = rawPhases.length > 0 ? rawPhases[rawPhases.length - 1].type : null;
    const phase = classifyTurn(turns[i], i, prevPhase, recentErrors, retryRanges);
    rawPhases.push({ type: phase, turn: i });
  }

  // Group consecutive same-type turns into phase segments
  const segments: Phase[] = [];
  let current: Phase | null = null;

  for (const { type, turn } of rawPhases) {
    if (current && current.type === type) {
      current.endTurn = turn;
    } else {
      if (current) segments.push(current);
      current = { type, startTurn: turn, endTurn: turn, badges: [] };
    }
  }
  if (current) segments.push(current);

  // Check for abandonment at the end
  if (segments.length > 0 && turns.length > 0) {
    const lastTurn = turns[turns.length - 1];
    const lastSegment = segments[segments.length - 1];

    // Abandonment detection
    const hasEndError = hasError(lastTurn) && lastTurn.role === "assistant";
    const hasTaskSwitch = lastTurn.role === "user" && /\b(let'?s move on|forget it|different approach|never mind)\b/i.test(lastTurn.content);
    const lastQuarter = turns.slice(Math.floor(turns.length * 0.8));
    const noWritesInLastQuarter = !lastQuarter.some((t) => hasTools(t, WRITE_TOOLS));

    if (hasEndError || hasTaskSwitch || (noWritesInLastQuarter && lastSegment.type !== "planning")) {
      // Only mark as abandonment if the session has errors
      const totalErrors = turns.filter(hasError).length;
      if (totalErrors > 0 || hasTaskSwitch) {
        if (lastSegment.type !== "abandonment") {
          const lastDisplayIdx = turns.length - 1;
          segments.push({
            type: "abandonment",
            startTurn: lastDisplayIdx,
            endTurn: lastDisplayIdx,
            badges: [],
          });
        }
      }
    }
  }

  return mergePhases(segments);
}

// ---------------------------------------------------------------------------
// Phase merging
// ---------------------------------------------------------------------------

function phaseLength(p: Phase): number {
  return p.endTurn - p.startTurn + 1;
}

/** Semantically significant phases that shouldn't be absorbed even at 1 turn. */
const SIGNIFICANT_PHASES = new Set<PhaseType>([
  "error", "user-correction", "retry-loop", "abandonment",
]);

function mergePhases(segments: Phase[]): Phase[] {
  if (segments.length <= 1) return segments;

  const merged: Phase[] = [];

  for (const seg of segments) {
    const len = phaseLength(seg);
    const prev = merged[merged.length - 1];

    // Rule 1: Merge consecutive same-type phases
    if (prev && prev.type === seg.type) {
      prev.endTurn = seg.endTurn;
      continue;
    }

    // Rule 3: Absorb micro-phases (<3 turns) into adjacent phase
    // Exception: significant phases are never absorbed
    if (len < 3 && !SIGNIFICANT_PHASES.has(seg.type) && prev) {
      // Absorb into previous phase with badge
      prev.endTurn = seg.endTurn;
      const existingBadge = prev.badges.find((b) => b.type === seg.type);
      if (existingBadge) {
        existingBadge.count += len;
      } else {
        prev.badges.push({ type: seg.type, count: len });
      }
      continue;
    }

    // Rule 2: Absorb 1-turn error between two same-type phases
    if (
      seg.type === "error" && len === 1 && prev && merged.length < segments.length
    ) {
      // Look ahead: if the next segment is the same type as prev, absorb
      const nextIdx = segments.indexOf(seg) + 1;
      if (nextIdx < segments.length && segments[nextIdx].type === prev.type) {
        prev.endTurn = seg.endTurn;
        const badge = prev.badges.find((b) => b.type === "error");
        if (badge) badge.count++;
        else prev.badges.push({ type: "error", count: 1 });
        continue;
      }
    }

    merged.push({ ...seg, badges: [...seg.badges] });
  }

  return merged;
}
