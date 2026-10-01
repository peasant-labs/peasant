'use client';

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { MoreHorizontal, RotateCw } from 'lucide-react';
import type { LocalPublication, LocalPublicationAudienceMember, LocalVillageCollective } from '@peasant-labs/schema';
import { Menu, PublishBar, PublishDialog, type MenuItem, type PublishDialogState } from '@/lib/ft-ui';
import { usePublishState } from '@/contexts/PublishContext';
import {
  fetchPublicationState,
  fetchVillageAuth,
  fetchVillageCollectives,
  isPublishingError,
  PublishingErrorCode,
  PublishingRequestError,
  startVillageSignIn,
} from '@/lib/share/publishing';
import { useRedactionPipeline, type RedactionScanTarget } from '@/lib/share/redactionScan';
import { DEFAULT_REDACTION_LEVEL } from '@/lib/share/redactions';
import {
  accessItems,
  accessSummary,
  addToDraft,
  barModel,
  changesSummary,
  collectiveNames,
  countNewTurns,
  currentReaderIds,
  initialDraft,
  NO_ACCESS_CHANGE,
  oneLine,
  pickerSuggestions,
  publishOutcome,
  pushRequest,
  removeFromDraft,
  restoreInDraft,
  reviewMatches,
  type AccessDraft,
  type PublishOutcome,
} from './publishModel';

/** How often the popup asks whether the browser sign-in finished. */
const SIGN_IN_POLL_MS = 2000;

/** The village the reader signs in to, when the server does not name one. */
function defaultVillageUrl(): string {
  return process.env.NEXT_PUBLIC_COMMONS_URL ?? 'https://village.peasantlabs.org';
}

/** Where village lists the collectives a reader can create or join. */
const VILLAGE_COLLECTIVES_PATH = '/groups';

/** The recovery when village no longer holds the transcript a receipt names. */
const TRANSCRIPT_MISSING = 'village no longer holds this transcript: run peasant village push --force for this session';

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

type PublicationRead =
  | { status: 'loading' }
  | { status: 'ready'; publication: LocalPublication | null; audienceKnown: boolean }
  | { status: 'error'; message: string; transcriptMissing: boolean };

/** Whether this computer is signed in to village, as far as the popup knows. */
type SignIn = 'unknown' | 'signed-in' | 'signed-out' | 'waiting';

/** A sign-in step that failed: checking the stored sign-in, or starting a new one. */
type SignInFailure = { step: 'check' | 'start'; message: string };

type CollectivesRead =
  | { status: 'idle' }
  | { status: 'loading' }
  | { status: 'ready'; collectives: LocalVillageCollective[] }
  | { status: 'error'; message: string };

/** A step before the push that stopped; retry runs it again. */
type Blocked = { stoppedAt: string; retry?: () => void };

export interface TranscriptPublishOptions {
  sessionId: string;
  /** The session title, kept in its case. */
  title: string;
  /** Every turn the page received, for the "N new turns" count and the scan version. */
  turns: readonly { timestamp: string }[];
  /** Whether the page has the session's detail yet; nothing is scanned before it. */
  loaded: boolean;
  /** The header's overflow actions, carried by the bar's more menu. */
  moreItems: MenuItem[];
  /** The route asked to open the popup on arrival (`?publish=open`). */
  openOnArrival: boolean;
  /** Called when the popup closes, so the route can drop its open request. */
  onArrivalHandled: () => void;
}

export interface TranscriptPublish {
  /** The header row's status, its one action, and the more menu. */
  bar: ReactNode;
  /** The publish popup, rendered beside the viewer. */
  dialog: ReactNode;
}

/**
 * The transcript page's publish flow: the bar in the header and the one popup
 * for publish, update and manage. The page owns the state and hands fairtrade's
 * controlled parts what they show; the scans and the publishes in flight live
 * in the app-level publish state, so they outlive this page.
 */
export function useTranscriptPublish(options: TranscriptPublishOptions): TranscriptPublish {
  const { sessionId, title, turns, loaded, moreItems, openOnArrival, onArrivalHandled } = options;
  const { redactionCache, updateRedactionCache, publishing, settled, publish } = usePublishState();

  // ── the publication state ────────────────────────────────────────────────
  const settledCount = settled.get(sessionId) ?? 0;
  const [readNonce, setReadNonce] = useState(0);
  const [read, setRead] = useState<PublicationRead>({ status: 'loading' });
  const reread = useCallback(() => {
    setRead({ status: 'loading' });
    setReadNonce((nonce) => nonce + 1);
  }, []);
  useEffect(() => {
    let live = true;
    fetchPublicationState(sessionId)
      .then(({ publication, audienceKnown }) => {
        if (live) setRead({ status: 'ready', publication, audienceKnown });
      })
      .catch((error: unknown) => {
        if (live) {
          setRead({
            status: 'error',
            message: messageOf(error),
            transcriptMissing: isPublishingError(error, PublishingErrorCode.VillageTranscriptMissing),
          });
        }
      });
    return () => {
      live = false;
    };
  }, [sessionId, settledCount, readNonce]);

  const publication = read.status === 'ready' ? read.publication : null;
  const publishedNow: 'publish' | 'update' = publication?.state === 'published' ? 'update' : 'publish';
  const newTurns = useMemo(() => countNewTurns(turns, publication?.publishedAt), [turns, publication?.publishedAt]);
  const publishingTo = publishing.get(sessionId);
  const readers = useMemo(() => currentReaderIds(publication), [publication]);

  // ── the popup ────────────────────────────────────────────────────────────
  const [open, setOpen] = useState(false);
  const [signIn, setSignIn] = useState<SignIn>('unknown');
  const [villageUrl, setVillageUrl] = useState<string | undefined>(undefined);
  const [signInFailure, setSignInFailure] = useState<SignInFailure | null>(null);
  // A sign-in that completes inside the popup changes what the publication read
  // reports (a signed-out computer reports every session unpublished).
  const sawSignedOut = useRef(false);
  const openGeneration = useRef(0);
  const [collectivesRead, setCollectivesRead] = useState<CollectivesRead>({ status: 'idle' });
  // Bumped to read the collectives again: on each open, and on retry.
  const [collectivesNonce, setCollectivesNonce] = useState(0);
  const [draft, setDraft] = useState<AccessDraft | null>(null);
  // The heading keeps the mode the popup opened in: a publish that stopped
  // after village took the content still reads as the publish the reader
  // started. Who can read it always follows what village holds now.
  const [openedAs, setOpenedAs] = useState<'publish' | 'update' | null>(null);
  const mode = openedAs ?? publishedNow;
  const [query, setQuery] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [outcome, setOutcome] = useState<PublishOutcome | null>(null);

  const openPopup = useCallback(() => {
    openGeneration.current += 1;
    setOutcome(null);
    setSignInFailure(null);
    setSignIn('unknown');
    sawSignedOut.current = false;
    setCollectivesRead({ status: 'idle' });
    setCollectivesNonce((nonce) => nonce + 1);
    setDraft(null);
    setOpenedAs(null);
    setQuery('');
    // Read the audience again: who can read it may have changed on village.
    reread();
    setOpen(true);
  }, [reread]);

  const arrived = useRef(false);
  useEffect(() => {
    if (!openOnArrival || arrived.current) return;
    arrived.current = true;
    openPopup();
  }, [openOnArrival, openPopup]);

  const close = useCallback(() => {
    openGeneration.current += 1;
    setOpen(false);
    setOutcome(null);
    setSignInFailure(null);
    if (openOnArrival) onArrivalHandled();
  }, [onArrivalHandled, openOnArrival]);

  // Whether this computer is signed in to village, asked each time it opens.
  useEffect(() => {
    if (!open || signIn !== 'unknown') return;
    let live = true;
    fetchVillageAuth()
      .then((auth) => {
        if (!live) return;
        setVillageUrl(auth.villageUrl || undefined);
        setSignIn(auth.authenticated ? 'signed-in' : 'signed-out');
      })
      .catch((error: unknown) => {
        if (live) setSignInFailure({ step: 'check', message: messageOf(error) });
      });
    return () => {
      live = false;
    };
  }, [open, signIn]);

  // A sign-in that completed while the popup was open: read the publication
  // again and start the draft over from it.
  useEffect(() => {
    if (signIn === 'signed-out' || signIn === 'waiting') {
      sawSignedOut.current = true;
      return;
    }
    if (signIn !== 'signed-in' || !sawSignedOut.current) return;
    sawSignedOut.current = false;
    setRead({ status: 'loading' });
    setDraft(null);
    setOpenedAs(null);
    reread();
  }, [reread, signIn]);

  // After `continue with github`, wait for the browser sign-in to land.
  useEffect(() => {
    if (!open || signIn !== 'waiting') return;
    let live = true;
    let timer = 0;
    const tick = async () => {
      try {
        const auth = await fetchVillageAuth();
        if (!live) return;
        if (auth.authenticated) {
          setVillageUrl(auth.villageUrl || undefined);
          setSignIn('signed-in');
          return;
        }
      } catch {
        // A missed poll is not a failed sign-in: the browser flow still runs.
      }
      if (!live) return;
      timer = window.setTimeout(tick, SIGN_IN_POLL_MS);
    };
    timer = window.setTimeout(tick, SIGN_IN_POLL_MS);
    return () => {
      live = false;
      window.clearTimeout(timer);
    };
  }, [open, signIn]);

  const connect = useCallback(() => {
    setSignInFailure(null);
    setSignIn('waiting');
    const generation = openGeneration.current;
    startVillageSignIn()
      .then((result) => {
        if (generation !== openGeneration.current) return;
        if (result.status === 'already_authenticated') setSignIn('unknown');
      })
      .catch((error: unknown) => {
        if (generation !== openGeneration.current) return;
        setSignIn('signed-out');
        setSignInFailure({ step: 'start', message: messageOf(error) });
      });
  }, []);

  // The collectives the reader can publish to, read once signed in. The read's
  // own status is not a dependency, so marking it loading never cancels it.
  useEffect(() => {
    if (!open || signIn !== 'signed-in') return;
    let live = true;
    setCollectivesRead({ status: 'loading' });
    fetchVillageCollectives(sessionId)
      .then((collectives) => {
        if (live) setCollectivesRead({ status: 'ready', collectives });
      })
      .catch((error: unknown) => {
        if (!live) return;
        if (isPublishingError(error, PublishingErrorCode.VillageSignedOut)) {
          setCollectivesRead({ status: 'idle' });
          setSignIn('signed-out');
          return;
        }
        setCollectivesRead({ status: 'error', message: messageOf(error) });
      });
    return () => {
      live = false;
    };
  }, [collectivesNonce, open, sessionId, signIn]);

  const collectives = useMemo(
    () => (collectivesRead.status === 'ready' ? collectivesRead.collectives : []),
    [collectivesRead],
  );

  // A first publish starts with the suggested collectives; an update with no change.
  useEffect(() => {
    if (!open || draft !== null || signIn !== 'signed-in' || collectivesRead.status !== 'ready' || read.status !== 'ready') return;
    setOpenedAs(publishedNow);
    setDraft(initialDraft(publishedNow, collectivesRead.collectives));
  }, [collectivesRead, draft, open, publishedNow, read.status, signIn]);

  // ── what leaves the machine: the cached local scan ───────────────────────
  // The scan starts when the popup opens and the session's detail is here,
  // whatever the sign-in: it runs on this computer. Its version is the turn
  // count, so a session that grew is scanned again; a failure stays until an
  // explicit re-scan.
  const scanTargets = useMemo<RedactionScanTarget[]>(
    () => (open && loaded ? [{ id: sessionId, version: String(turns.length) }] : []),
    [loaded, open, sessionId, turns.length],
  );
  const scan = useRedactionPipeline(scanTargets, DEFAULT_REDACTION_LEVEL, false, redactionCache, updateRedactionCache);
  const scanResult = scan.sessionRedactions.get(sessionId);
  const scanClean = scan.phase === 'ready' && scan.scanError == null && scanResult !== undefined;
  const scanProp = scan.scanError != null
    ? { failure: `the scan failed, so publish is off. ${scan.scanError}` }
    : scanResult
      ? { matches: reviewMatches(sessionId, scanResult), total: scanResult.length, matchCount: scan.sessionMatchCounts.get(sessionId) }
      : undefined;

  // ── who can read it ──────────────────────────────────────────────────────
  const activeDraft = draft ?? NO_ACCESS_CHANGE;
  const access = useMemo(
    () => accessItems({ publication, collectives, draft: activeDraft }),
    [activeDraft, collectives, publication],
  );
  const names = useMemo(() => collectiveNames(collectives, publication), [collectives, publication]);
  const suggestions = useMemo(() => pickerSuggestions(collectives, access, query), [access, collectives, query]);
  const readerCount = access.filter((item) => item.pending !== 'removal').length;
  const listed = useMemo(() => access.map((item) => item.id), [access]);

  // A step before the push that stopped, and how to retry it. The publication
  // and its audience must be known before anything is offered: a popup that
  // guessed them would publish as a first publish, or update a list it cannot
  // show.
  const blocked: Blocked | null = signInFailure != null
    ? {
      stoppedAt: signInFailure.step === 'check'
        ? `checking this computer’s village sign-in: ${oneLine(signInFailure.message)}`
        : `starting the github sign-in: ${oneLine(signInFailure.message)}`,
      retry: () => { setSignInFailure(null); setSignIn('unknown'); },
    }
    : read.status === 'error'
      ? {
        stoppedAt: read.transcriptMissing ? `reading whether this is published: ${TRANSCRIPT_MISSING}` : `reading whether this is published: ${oneLine(read.message)}`,
        retry: read.transcriptMissing ? undefined : reread,
      }
      : read.status === 'ready' && publication === null
        ? { stoppedAt: 'reading this session: it is not recorded on this computer yet; run peasant ingest, then retry', retry: reread }
        : read.status === 'ready' && publication?.state === 'published' && !read.audienceKnown
        ? { stoppedAt: 'reading who can read it on village', retry: reread }
        : collectivesRead.status === 'error'
          ? { stoppedAt: `reading your collectives on village: ${oneLine(collectivesRead.message)}`, retry: () => setCollectivesNonce((nonce) => nonce + 1) }
          : null;

  // Publish only from a clean scan of the content now on the page, signed in,
  // with what village holds known.
  // An update can be sent before the collectives list arrives: its draft is
  // "no change" until the reader makes one.
  const canPublish = scanClean && signIn === 'signed-in' && blocked === null && read.status === 'ready'
    && (draft !== null || publishedNow === 'update');

  const runPublish = useCallback(async () => {
    if (!canPublish || publishingTo !== undefined || submitting) return;
    const request = pushRequest(sessionId, DEFAULT_REDACTION_LEVEL, activeDraft, { listed, readers });
    const audienceBefore = publication?.audience ?? [];
    setOutcome(null);
    setSubmitting(true);
    try {
      const response = await publish(sessionId, request, readerCount);
      // Who can read it now comes from the same read the bar uses.
      let audienceAfter: readonly LocalPublicationAudienceMember[] | undefined;
      try {
        const after = await fetchPublicationState(sessionId);
        // The popup's rows follow this read at once, so a reader removed on
        // the next screen is a reader the push takes the transcript back from.
        setRead({ status: 'ready', publication: after.publication, audienceKnown: after.audienceKnown });
        if (after.audienceKnown && after.publication?.state === 'published') audienceAfter = after.publication.audience ?? [];
      } catch {
        // The bar re-reads on its own; the push's steps stand in here.
      }
      setOutcome(publishOutcome({
        response,
        sessionId,
        names,
        audienceBefore,
        audienceAfter,
        requestedAdds: request.collectives?.add ?? [],
        fallbackUrl: publication?.transcriptUrl,
      }));
      // The draft now describes what was asked; a retry starts from what
      // village holds, keeping the adds village did not take.
      const removed = new Set(response.sessions.find((item) => item.sessionId === sessionId)?.steps
        ?.filter((step) => step.step === 'remove_collective' && step.outcome === 'succeeded')
        .map((step) => step.collectiveId) ?? []);
      setDraft((current) => (current ? { add: current.add, remove: current.remove.filter((id) => !removed.has(id)) } : current));
    } catch (error) {
      if (error instanceof PublishingRequestError && error.status === 401) {
        setSignIn('signed-out');
      } else {
        setOutcome({ kind: 'stopped', stoppedAt: `sending it to village: ${oneLine(messageOf(error))}` });
      }
    } finally {
      setSubmitting(false);
    }
  }, [activeDraft, canPublish, listed, names, publication, publish, readerCount, readers, sessionId, publishingTo, submitting]);

  // The order matters: a finished publish shows its link; anything that stops
  // the flow before the push (a failed read, a sign-in) comes next; then the
  // scan, so a re-scan that fails or runs after a stop turns publish and retry
  // off; and a stopped push last.
  const dialogState: PublishDialogState = (() => {
    if (publishingTo !== undefined || submitting) return 'publishing';
    if (outcome && outcome.kind !== 'stopped') return outcome.kind;
    if (blocked) return 'stopped';
    if (signIn === 'waiting') return 'waiting-github';
    if (signIn === 'signed-out') return 'connect';
    if (signIn === 'unknown' || read.status === 'loading' || scan.phase === 'scanning') return 'checking';
    if (scan.scanError != null) return 'scan-failed';
    if (outcome?.kind === 'stopped') return 'stopped';
    if (collectivesRead.status === 'ready' && collectivesRead.collectives.length === 0 && access.length === 0) return 'no-collective';
    return 'ready';
  })();
  const stopped = dialogState === 'stopped'
    ? (blocked ?? (outcome?.kind === 'stopped' ? { stoppedAt: outcome.stoppedAt, retry: /push --force/.test(outcome.stoppedAt) ? undefined : () => { void runPublish(); } } : null))
    : null;

  const bar = read.status === 'ready' && read.publication
    ? (
      <PublishBar
        {...barModel({ publication: read.publication, audienceKnown: read.audienceKnown, newTurns, publishingTo })}
        onAction={openPopup}
        moreItems={moreItems}
      />
    )
    : (
      <span role="group" aria-label="publish" className="inline-flex items-center gap-3">
        {read.status === 'error' && (
          <>
            <span role="alert" className="font-mono text-[14px] text-ink-2" title={read.message}>
              {read.transcriptMissing ? TRANSCRIPT_MISSING : 'the publish state could not be read'}
            </span>
            {!read.transcriptMissing && <button type="button" className="btn btn-ghost btn-sm" onClick={reread}>
              <RotateCw size={14} aria-hidden="true" /> retry
            </button>}
          </>
        )}
        <Menu icon={MoreHorizontal} ariaLabel="more" size="sm" align="end" items={moreItems} />
      </span>
    );

  const dialog = (
    <PublishDialog
      open={open}
      onClose={close}
      title={title}
      mode={mode}
      state={dialogState}
      scan={scanProp}
      onRescan={() => scan.runScan(true)}
      changes={mode === 'update' ? { summary: changesSummary(newTurns) } : undefined}
      access={access}
      onRemove={(id) => setDraft((current) => removeFromDraft(current ?? NO_ACCESS_CHANGE, id, readers))}
      onRestore={(id) => setDraft((current) => restoreInDraft(current ?? NO_ACCESS_CHANGE, id))}
      picker={collectivesRead.status === 'ready'
        ? {
          suggestions,
          query,
          onQueryChange: setQuery,
          onAdd: (id: string) => {
            setDraft((current) => addToDraft(current ?? NO_ACCESS_CHANGE, id));
            setQuery('');
          },
        }
        : undefined}
      // The auto-publish checkbox saves an auto-publish rule through the
      // settings routes, which this build does not serve yet; it is left out
      // rather than shown as a control that saves nothing.
      accessSummary={mode === 'update' ? accessSummary(activeDraft, names) : undefined}
      onPublish={() => { void runPublish(); }}
      onConnect={connect}
      joinHref={`${villageUrl ?? defaultVillageUrl()}${VILLAGE_COLLECTIVES_PATH}`}
      stoppedAt={stopped?.stoppedAt}
      onRetry={stopped?.retry}
      done={outcome && outcome.kind !== 'stopped' ? outcome.done : undefined}
    />
  );

  return { bar, dialog };
}
