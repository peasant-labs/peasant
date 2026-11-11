'use client';

import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';
import { Breadcrumbs } from '@/components/Breadcrumbs';
import { Button, FeedbackPanel, Skeleton } from '@/lib/ft-ui';
import { kickstartCopy } from '@/lib/kickstart/copy';
import type { KickstartSession, KickstartStop } from '@/lib/kickstart/model';
import { useKickstartSession } from '@/lib/kickstart/useKickstartSession';
import { useVillageSignIn } from '@/lib/kickstart/useVillageSignIn';
import {
  connectOfferStopKey,
  firstVisibleStop,
  nextVisibleStop,
  previousVisibleStop,
  stopByKey,
  visibleStopAfter,
  visibleStops,
} from '@/lib/kickstart/view';
import { KickstartCloseReason, KickstartStopKind, type KickstartScope } from '@/lib/kickstart/values';
import { ConnectOffer } from './ConnectOffer';
import { FinishPanel } from './FinishPanel';
import { StaleNotice } from './StaleNotice';
import { StepFrame } from './StepFrame';
import { StopRenderer } from './StopRenderer';
import './kickstart.css';

/**
 * The stop the page is on. The server lists the stops and says which are
 * visible; this keeps only the reader's place among them. A stop the server
 * hides while the reader is on it moves the reader to the next visible stop.
 */
function placeOf(session: KickstartSession, key: string | null): KickstartStop | undefined {
  const visible = visibleStops(session);
  if (key === null) return visible[0];
  const same = visible.find((stop) => stop.key === key);
  if (same !== undefined) return same;
  if (stopByKey(session, key) !== undefined) return visibleStopAfter(session, key) ?? visible[visible.length - 1];
  return firstVisibleStop(session);
}

export interface KickstartFlowProps {
  scope: KickstartScope;
  /** The page's one heading. */
  title: string;
  /** The trail above it, after `home`. */
  trail: readonly { label: string; href?: string }[];
  /** The wizard's accessible name. */
  ariaLabel: string;
}

/**
 * One live setup session, drawn stop by stop. The wizard (`scope: full`) and
 * the standalone selection page (`scope: selection`) are this component with a
 * different scope: the scope decides which stops the server serves, and
 * nothing else changes.
 */
export function KickstartFlow({ scope, title, trail, ariaLabel }: KickstartFlowProps) {
  const api = useKickstartSession(scope);
  const copy = kickstartCopy(scope);
  const signIn = useVillageSignIn();
  const { session, ending } = api;
  const [currentKey, setCurrentKey] = useState<string | null>(null);
  const [visited, setVisited] = useState<ReadonlySet<string>>(() => new Set());

  const committed = ending !== null && ending.committed !== null;
  const stop = useMemo(() => {
    if (session === null) return undefined;
    if (committed) {
      return session.stops.find((candidate) => candidate.kind === KickstartStopKind.Ingest && candidate.visible) ?? placeOf(session, currentKey);
    }
    return placeOf(session, currentKey);
  }, [committed, currentKey, session]);

  useEffect(() => {
    if (stop === undefined) return;
    setVisited((current) => (current.has(stop.key) ? current : new Set(current).add(stop.key)));
  }, [stop]);

  const go = useCallback((target: KickstartStop | undefined) => {
    if (target !== undefined) setCurrentKey(target.key);
  }, []);

  const frame = (body: ReactNode) => (
    <div className="asb-page ks-page animate-fade-up">
      <Breadcrumbs items={[{ label: 'home', href: '/' }, ...trail]} />
      <h1 className="iu-page-title page-title">{title}</h1>
      {body}
    </div>
  );

  if (api.openError !== null) {
    return frame(
      <FeedbackPanel variant="error" title={copy.openFailed}>
        <span>{api.openError.message}</span>
        <Button variant="secondary" size="sm" onClick={() => void api.reopen()}>
          try again
        </Button>
      </FeedbackPanel>,
    );
  }
  if (session === null || stop === undefined) {
    return frame(<Skeleton label={copy.opening} lines={3} />);
  }

  const visible = visibleStops(session);
  const index = visible.findIndex((candidate) => candidate.key === stop.key);
  const offerKey = connectOfferStopKey(session);
  const offerHere = offerKey === stop.key && ending === null;
  const onReview = stop.kind === KickstartStopKind.Review;
  const next = nextVisibleStop(session, stop.key);
  const previous = previousVisibleStop(session, stop.key);
  const expired = ending !== null && ending.reason === KickstartCloseReason.Expired;

  const steps = visible.map((candidate) => ({ id: candidate.key, label: candidate.title }));
  const completed = new Set(visible.slice(0, Math.max(index, 0)).map((candidate) => candidate.key));
  if (committed) completed.add(stop.key);
  const reachable = new Set(
    visible
      .filter((candidate) => (visited.has(candidate.key) || completed.has(candidate.key)) && candidate.kind !== KickstartStopKind.Ingest)
      .map((candidate) => candidate.key),
  );

  const notices = (
    <>
      {(api.overwritten.length > 0 || api.answerError !== null || api.commitError !== null || api.refusal.length > 0) && (
        <div className="ks-notices">
          <StaleNotice session={session} overwritten={api.overwritten} dismiss={api.dismissOverwritten} />
          {api.refusal.length > 0 && (
            <div className="ks-notice" role="alert">
              {copy.refused(api.refusal.length)}
            </div>
          )}
          {api.answerError !== null && (
            <div className="ks-notice" role="alert">
              {api.answerError.message}
            </div>
          )}
          {api.commitError !== null && (
            <div className="ks-notice" role="alert">
              {api.commitError.message}
            </div>
          )}
        </div>
      )}
    </>
  );

  let body: ReactNode;
  if (expired) {
    body = (
      <FeedbackPanel variant="empty" title={copy.expired.title}>
        <span>{copy.expired.body}</span>
        <Button variant="primary" size="sm" onClick={() => void api.reopen()}>
          {copy.expired.reopen}
        </Button>
      </FeedbackPanel>
    );
  } else if (committed && ending !== null && ending.committed !== null) {
    body = (
      <FinishPanel
        copy={copy}
        committed={ending.committed}
        ingest={api.ingest ?? ending.committed.ingest}
        importNow={() => void api.importNow()}
        error={api.importError}
      />
    );
  } else if (offerHere) {
    body = <ConnectOffer connect={session.connect} signIn={signIn} answer={api.answerConnect} />;
  } else {
    body = (
      <StopRenderer
        stop={stop}
        session={session}
        api={api}
        copy={copy}
        signIn={signIn}
        answeredConnection={() => go(nextVisibleStop(session, stop.key))}
      />
    );
  }

  const settled = ending === null;
  const primary = !settled || offerHere
    ? null
    : onReview
      ? { label: 'save and import', onClick: () => void api.commit(), loading: api.committing, disabled: api.committing }
      : next !== undefined
        ? { label: 'continue', onClick: () => go(next) }
        : null;

  return frame(
    <StepFrame
      ariaLabel={ariaLabel}
      steps={steps}
      current={stop.key}
      completed={completed}
      reachable={reachable}
      onJump={(key) => go(stopByKey(session, key))}
      notices={notices}
      back={settled && previous !== undefined ? () => go(previous) : null}
      primary={primary}
    >
      {body}
    </StepFrame>,
  );
}
