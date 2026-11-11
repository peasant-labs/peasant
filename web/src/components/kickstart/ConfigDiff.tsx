'use client';

import { DiffView } from '@/lib/ft-ui';
import type { KickstartDiff } from '@/lib/kickstart/model';
import { KickstartDiffLineType } from '@/lib/kickstart/values';

/**
 * The server's structured before and after of config.yaml, collapsed to one
 * line that carries the file and its churn. Opening it shows fairtrade's
 * DiffView over the hunks as they came: this component computes no diff and
 * draws no line itself.
 */
export function ConfigDiff({ diff }: { diff: KickstartDiff }) {
  let added = 0;
  let removed = 0;
  for (const hunk of diff.hunks) {
    for (const line of hunk.lines) {
      if (line.type === KickstartDiffLineType.Add) added += 1;
      else if (line.type === KickstartDiffLineType.Del) removed += 1;
    }
  }
  const hunks = diff.hunks.map((hunk) => ({
    header: hunk.header,
    lines: hunk.lines.map((line) => ({
      type: line.type.wire,
      ...(line.oldNo !== null ? { oldNo: line.oldNo } : {}),
      ...(line.newNo !== null ? { newNo: line.newNo } : {}),
      text: line.text,
    })),
  }));
  return (
    <details className="ks-diff">
      <summary>
        <span>exact changes to {diff.file}</span>
        <span className="ks-num">
          <span className="ks-churn-add">+{added}</span> <span className="ks-churn-del">−{removed}</span>
        </span>
      </summary>
      <DiffView file={diff.file} hunks={hunks} />
      {diff.truncated && (
        <p className="ks-help" role="status">
          the list of changes was cut short. every changed value still appears above and is saved.
        </p>
      )}
    </details>
  );
}
