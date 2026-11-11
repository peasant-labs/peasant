'use client';

import Link from 'next/link';
import { Button, Spinner } from '@/lib/ft-ui';
import type { KickstartCopy } from '@/lib/kickstart/copy';
import type { KickstartCommitted, KickstartIngest } from '@/lib/kickstart/model';
import { KickstartIngestState } from '@/lib/kickstart/values';

const countFormat = new Intl.NumberFormat('en-US');

/**
 * What happened after the save, and what to do next. The import's progress is
 * pushed on the topic, so it advances here without a reload and keeps going if
 * this page is closed. The next steps are the server's list, each a link: none
 * installs a hook, none publishes, and no control here does either.
 */
export function FinishPanel({
  copy,
  committed,
  ingest,
  importNow,
  error,
}: {
  copy: KickstartCopy;
  committed: KickstartCommitted;
  ingest: KickstartIngest;
  importNow: () => void;
  error: Error | null;
}) {
  const running = ingest.state === KickstartIngestState.Running;
  return (
    <section className="ks-finish" aria-labelledby="ks-finish-title">
      <h2 className="ks-title" id="ks-finish-title">
        {copy.saved}
      </h2>
      <p className="ks-intro">
        saved to <code>{committed.configPath}</code>. nothing was published and no hook was installed.
      </p>

      <div className="ks-scan" role="status">
        <span className="ks-field-label">{ingest.state.label}</span>
        {running && <Spinner>reading your selected sessions</Spinner>}
        <span className="ks-counts">
          <span className="ks-num">{countFormat.format(ingest.new)}</span> new,{' '}
          <span className="ks-num">{countFormat.format(ingest.updated)}</span> updated
        </span>
        {ingest.note !== null && <span className="ks-help">{ingest.note}</span>}
        {ingest.error !== null && (
          <p className="ks-refusal" role="alert">
            {ingest.error}
          </p>
        )}
        <div className="ks-answers">
          <Button variant="secondary" size="sm" disabled={running} onClick={importNow}>
            import now
          </Button>
        </div>
        {error !== null && (
          <p className="ks-refusal" role="alert">
            {error.message}
          </p>
        )}
      </div>

      {committed.diagnostics.length > 0 && (
        <section aria-labelledby="ks-diagnostics-title" className="ks-field">
          <h3 className="ks-field-label" id="ks-diagnostics-title">
            notes from reading your sessions
          </h3>
          <ul className="ks-diagnostics">
            {committed.diagnostics.map((line, index) => (
              <li key={`${index}:${line}`}>{line}</li>
            ))}
          </ul>
        </section>
      )}

      <section aria-labelledby="ks-next-title" className="ks-field">
        <h3 className="ks-field-label" id="ks-next-title">
          what to do next
        </h3>
        <ul className="ks-steps">
          {committed.nextSteps.map((step) => (
            <li key={step.wire} className="ks-step">
              <Link className="ks-step-name" href={step.href} aria-describedby={`ks-step-${step.wire}`}>
                {step.label}
              </Link>
              <span className="ks-step-detail" id={`ks-step-${step.wire}`}>
                {step.detail}
              </span>
            </li>
          ))}
        </ul>
      </section>
    </section>
  );
}
