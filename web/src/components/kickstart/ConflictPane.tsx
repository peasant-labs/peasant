'use client';

import { Button } from '@/lib/ft-ui';
import type { KickstartCopy } from '@/lib/kickstart/copy';
import type { KickstartConflict, KickstartSession } from '@/lib/kickstart/model';
import { CONFLICT_BLOCK_OPEN_LINES, conflictSides, type ConflictValue } from '@/lib/kickstart/view';
import { ALL_KICKSTART_RESOLUTIONS, type KickstartResolution } from '@/lib/kickstart/values';

/**
 * One side of a row. A scalar is a phrase. An object or a list is the server's
 * value as indented lines in a mono block, and a long one is collapsed so a
 * large selection does not push the choice buttons off the page.
 */
function Side({ value, name }: { value: ConflictValue; name: string }) {
  if (value.phrase !== null) return <>{value.phrase}</>;
  const block = (
    <pre className="ks-value" role="group" aria-label={name} tabIndex={0}>
      <code>{value.lines.join('\n')}</code>
    </pre>
  );
  if (value.lines.length <= CONFLICT_BLOCK_OPEN_LINES) return block;
  return (
    <details className="ks-value-details">
      <summary>
        <span className="ks-num">show {value.lines.length} lines</span>
      </summary>
      {block}
    </details>
  );
}

/**
 * Answered fields whose saved value changed while the wizard was open. Each
 * row shows both sides and the two ways to settle it; the draft is saved only
 * after every row is settled, and the server says which rows remain. Fields
 * the person never answered are never listed.
 */
export function ConflictPane({
  copy,
  session,
  rows,
  resolve,
}: {
  copy: KickstartCopy;
  session: KickstartSession;
  rows: readonly KickstartConflict[];
  resolve: (field: string, resolution: KickstartResolution) => void;
}) {
  if (rows.length === 0) return null;
  return (
    <section className="ks-field" aria-labelledby="ks-conflicts-title">
      <h3 className="ks-field-label" id="ks-conflicts-title">
        {copy.conflicts.title}
      </h3>
      <p className="ks-help">{copy.conflicts.help}</p>
      <ul className="ks-conflicts">
        {rows.map((row) => {
          const sides = conflictSides(session, row);
          return (
            <li key={row.field} className="ks-conflict">
              <span className="ks-field-label">{row.label}</span>
              <dl className="ks-conflict-sides">
                <div className="ks-side">
                  <dt>your answer</dt>
                  <dd>
                    <Side value={sides.mine} name={`your answer, ${row.label}`} />
                  </dd>
                </div>
                <div className="ks-side">
                  <dt>saved now</dt>
                  <dd>
                    <Side value={sides.theirs} name={`saved now, ${row.label}`} />
                  </dd>
                </div>
              </dl>
              <div className="ks-answers">
                {ALL_KICKSTART_RESOLUTIONS.map((resolution) => (
                  <Button
                    key={resolution.wire}
                    variant="secondary"
                    size="sm"
                    aria-label={`${resolution.label} for ${row.label}`}
                    onClick={() => resolve(row.field, resolution)}
                  >
                    {resolution.label}
                  </Button>
                ))}
              </div>
            </li>
          );
        })}
      </ul>
    </section>
  );
}
