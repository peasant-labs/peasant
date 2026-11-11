'use client';

import type { KickstartConflict, KickstartSession, KickstartStop } from '@/lib/kickstart/model';
import type { KickstartSessionApi } from '@/lib/kickstart/useKickstartSession';
import { ConfigDiff } from './ConfigDiff';
import { ConflictPane } from './ConflictPane';
import { NoticeText } from './ChoiceStep';

/**
 * The review stop, in the order the server's answer deserves: the rows that
 * still need a choice, then the summary the server composed, then the exact
 * file changes, collapsed. The page writes none of the sentences.
 */
export function ReviewStep({
  stop,
  session,
  api,
}: {
  stop: KickstartStop;
  session: KickstartSession;
  api: KickstartSessionApi;
}) {
  // The session lists the rows it still holds. A commit the server refused names
  // the same rows; they are shown until the session answers with its own.
  const rows: readonly KickstartConflict[] = session.conflicts.length > 0 ? session.conflicts : api.refusal;
  return (
    <>
      {stop.fields.map((field) => (
        <NoticeText key={field.key} field={field} />
      ))}
      <ConflictPane session={session} rows={rows} resolve={(field, resolution) => void api.resolve(field, resolution)} />
      {stop.summary !== null && (
        <section className="ks-summary" aria-label="what saving does">
          {stop.summary.values.length > 0 && (
            <div className="ks-summary-group">
              <span className="ks-field-label">your choices</span>
              <ul>
                {stop.summary.values.map((line) => (
                  <li key={line}>{line}</li>
                ))}
              </ul>
            </div>
          )}
          {stop.summary.effects.length > 0 && (
            <div className="ks-summary-group">
              <span className="ks-field-label">what this does</span>
              <ul>
                {stop.summary.effects.map((line) => (
                  <li key={line}>{line}</li>
                ))}
              </ul>
            </div>
          )}
        </section>
      )}
      {stop.diff !== null && <ConfigDiff diff={stop.diff} />}
    </>
  );
}
