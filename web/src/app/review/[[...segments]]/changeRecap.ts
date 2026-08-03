import type { DecodedChangeDetailPayload } from '@/lib/api/map';
import { groupChangedFiles, summarizeChange } from './signals';

/**
 * Per-PR recap (roadmap 5.5): a shareable plain-markdown summary of a change,
 * for pasting into a PR description / issue / standup. It is an EXPORT affordance
 * (the on-screen caption stays the live view) — the analogue of the viewer's
 * copy-as-markdown (4.7), not a second on-screen panel, so it doesn't duplicate
 * the caption/footnotes.
 *
 * Pure function of the payload: deterministic, no clock/random, no React. Every
 * line is a NEUTRAL fact (files, churn, where it landed, recorded work, recurring
 * friction) — never a verdict/grade/cost. ASCII only (portable in any PR box).
 */

/** Top directory groups (most files first) as "dir (n)", capped. */
function whereLanded(payload: DecodedChangeDetailPayload, cap = 3): string {
  return groupChangedFiles(payload)
    .slice(0, cap)
    .map((g) => `${g.dir} (${g.files.length})`)
    .join(', ');
}

export function buildChangeRecap(payload: DecodedChangeDetailPayload): string {
  const lines: string[] = [`## ${payload.branch}`, ''];

  const n = payload.files.length;
  const churn =
    payload.linesAdded > 0 || payload.linesRemoved > 0
      ? ` (+${payload.linesAdded} / -${payload.linesRemoved})`
      : '';
  if (n === 0) {
    lines.push('No file changes.');
  } else {
    const where = whereLanded(payload);
    lines.push(`${n} file${n === 1 ? '' : 's'} changed${churn}${where ? ` across ${where}` : ''}.`);
  }

  const s = summarizeChange(payload);
  if (s.sessions > 0) {
    lines.push(
      '',
      `Recorded work: ${s.sessions} conversation${s.sessions === 1 ? '' : 's'}, ${s.tasks} request${s.tasks === 1 ? '' : 's'}.`,
    );
  }

  if (payload.frictions.length > 0) {
    lines.push('', 'Recurring friction:');
    for (const f of payload.frictions) {
      lines.push(
        `- ${f.file} · ${f.label} ${f.count} time${f.count === 1 ? '' : 's'} across ${f.sessions} conversation${f.sessions === 1 ? '' : 's'}`,
      );
    }
  }

  return lines.join('\n') + '\n';
}
