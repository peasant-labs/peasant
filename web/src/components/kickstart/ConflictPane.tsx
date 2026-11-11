'use client';

import { Button } from '@/lib/ft-ui';
import type { KickstartConflict, KickstartSession } from '@/lib/kickstart/model';
import { conflictSides } from '@/lib/kickstart/view';
import { ALL_KICKSTART_RESOLUTIONS, type KickstartResolution } from '@/lib/kickstart/values';

/**
 * Answered fields whose saved value changed while the wizard was open. Each
 * row shows both sides and the two ways to settle it; the draft is saved only
 * after every row is settled, and the server says which rows remain. Fields
 * the person never answered are never listed.
 */
export function ConflictPane({
  session,
  rows,
  resolve,
}: {
  session: KickstartSession;
  rows: readonly KickstartConflict[];
  resolve: (field: string, resolution: KickstartResolution) => void;
}) {
  if (rows.length === 0) return null;
  return (
    <section className="ks-field" aria-labelledby="ks-conflicts-title">
      <h3 className="ks-field-label" id="ks-conflicts-title">
        saved values that changed while you set up
      </h3>
      <p className="ks-help">
        these were changed in your saved settings after you answered them here. choose which value to keep for each one.
      </p>
      <ul className="ks-conflicts">
        {rows.map((row) => {
          const sides = conflictSides(session, row);
          return (
            <li key={row.field} className="ks-conflict">
              <span className="ks-field-label">{row.label}</span>
              <dl className="ks-conflict-sides">
                <div className="ks-side">
                  <dt>your answer</dt>
                  <dd>{sides.mine}</dd>
                </div>
                <div className="ks-side">
                  <dt>saved now</dt>
                  <dd>{sides.theirs}</dd>
                </div>
              </dl>
              <div className="ks-actions">
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
