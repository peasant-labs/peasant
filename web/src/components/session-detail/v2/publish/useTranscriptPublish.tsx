'use client';

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { MoreHorizontal, RotateCw } from 'lucide-react';
import type { LocalPublication, LocalVillageCollective } from '@peasant-labs/schema';
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
  initialDraft,
  NO_ACCESS_CHANGE,
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
/** The server waits this long for the GitHub sign-in, and so does the popup. */
const SIGN_IN_TIMEOUT_MS = 3 * 60 * 1000;

/** The village the reader signs in to, when the server does not name one. */
function defaultVillageUrl(): string {
  return process.env.NEXT_PUBLIC_COMMONS_URL ?? 'https://village.peasantlabs.org';
}

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

type PublicationRead =
  | { status: 'loading' }
  | { status: 'ready'; publication: LocalPublication | null; audienceKnown: boolean }
  | { status: 'error'; message: string };

/** Whether this computer is signed in to village, as far as the popup knows. */
type SignIn = 'unknown' | 'signed-in' | 'signed-out' | 'waiting';

type CollectivesRead =
  | { status: 'idle' }
  | { status: 'loading' }
  | { status: 'ready'; collectives: LocalVillageCollective[] }
  | { status: 'error'; message: string };

/** A step before the push that stopped; retry runs it again. */
type Blocked = { stoppedAt: string; retry: () => void };

export interface TranscriptPublishOptions {
  sessionId: string;
  /** The session title, kept in its case. */
  title: string;
  /** Every turn the page received, for the "N new turns" count and the scan version. */
  turns: readonly { timestamp: string }[];
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
  const { sessionId, title, turns, moreItems, openOnArrival, onArrivalHandled } = options;
  const { redactionCache, updateRedactionCache, publishing, settled, publish } = usePublishState();

  // ── the publication state ────────────────────────────────────────────────
  const settledCount = settled.get(sessionId) ?? 0;
  const [readNonce, setReadNonce] = useState(0);
  const [read, setRead] = useState<PublicationRead>({ status: 'loading' });
  useEffect(() => {
    let live = true;
    fetchPublicationState(sessionId)
      .then(({ publication, audienceKnown }) => {
        if (live) setRead({ status: 'ready', publication, audienceKnown });
      })
      .catch((error: unknown) => {
        if (live) setRead({ status: 'error', message: messageOf(error) });
      });
    return () => {
      live = false;
    };
  }, [sessionId, settledCount, readNonce]);

  const publication = read.status === 'ready' ? read.publication : null;
  const publishedNow: 'publish' | 'update' = publication?.state === 'published' ? 'update' : 'publish';
  const newTurns = useMemo(() => countNewTurns(turns, publication?.publishedAt), [turns, publication?.publishedAt]);
  const publishingTo = publishing.get(sessionId);

  // ── the popup ────────────────────────────────────────────────────────────
  const [open, setOpen] = useState(false);
  const [signIn, setSignIn] = useState<SignIn>('unknown');
  const [villageUrl, setVillageUrl] = useState<string | undefined>(undefined);
  const [signInFailure, setSignInFailure] = useState<string | null>(null);
  const [collectivesRead, setCollectivesRead] = useState<CollectivesRead>({ status: 'idle' });
  // Bumped to read the collectives again: on each open, and on retry.
  const [collectivesNonce, setCollectivesNonce] = useState(0);
  const [draft, setDraft] = useState<AccessDraft | null>(null);
  // The popup keeps the mode it opened in: a publish that stopped after village
  // took the content still retries as the publish the reader started.
  const [openedAs, setOpenedAs] = useState<'publish' | 'update' | null>(null);
  const mode = openedAs ?? publishedNow;
  const [query, setQuery] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [outcome, setOutcome] = useState<PublishOutcome | null>(null);

  const openPopup = useCallback(() => {
    setOutcome(null);
    setSignInFailure(null);
    setSignIn('unknown');
    setCollectivesRead({ status: 'idle' });
    setCollectivesNonce((nonce) => nonce + 1);
    setDraft(null);
    setOpenedAs(null);
    setQuery('');
    // Read the audience again: who can read it may have changed on village.
    setReadNonce((nonce) => nonce + 1);
    setOpen(true);
  }, []);

  const arrived = useRef(false);
  useEffect(() => {
    if (!openOnArrival || arrived.current) return;
    arrived.current = true;
    openPopup();
  }, [openOnArrival, openPopup]);

  const close = useCallback(() => {
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
        if (live) setSignInFailure(messageOf(error));
      });
    return () => {
      live = false;
    };
  }, [open, signIn]);

  // After `continue with github`, wait for the browser sign-in to land.
  useEffect(() => {
    if (!open || signIn !== 'waiting') return;
    let live = true;
    const started = Date.now();
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
      if (Date.now() - started >= SIGN_IN_TIMEOUT_MS) {
        setSignIn('signed-out');
        return;
      }
      timer = window.setTimeout(tick, SIGN_IN_POLL_MS);
    };
    timer = window.setTimeout(tick, SIGN_IN_POLL_MS);
    return () => {
      live = false;
      window.clearTimeout(timer);
    };
  }, [open, signIn]);

  const connect = useCallback(() => {
    setSignIn('waiting');
    startVillageSignIn()
      .then((result) => {
        if (result.status === 'already_authenticated') setSignIn('unknown');
      })
      .catch((error: unknown) => {
        setSignIn('signed-out');
        setSignInFailure(messageOf(error));
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
    if (!open || draft !== null || collectivesRead.status !== 'ready' || read.status !== 'ready') return;
    setOpenedAs(publishedNow);
    setDraft(initialDraft(publishedNow, collectivesRead.collectives));
  }, [collectivesRead, draft, open, publishedNow, read.status]);

  // ── what leaves the machine: the cached local scan ───────────────────────
  // The scan starts when the popup opens, whatever the sign-in: it runs on this
  // computer. Its version is the turn count, so a session that grew is scanned
  // again; a failure stays until an explicit re-scan.
  const scanTargets = useMemo<RedactionScanTarget[]>(
    () => (open ? [{ id: sessionId, version: String(turns.length) }] : []),
    [open, sessionId, turns.length],
  );
  const scan = useRedactionPipeline(scanTargets, DEFAULT_REDACTION_LEVEL, false, redactionCache, updateRedactionCache);
  const scanResult = scan.sessionRedactions.get(sessionId);
  const scanProp = scan.scanError != null
    ? { failure: `the scan failed, so publish is off. ${scan.scanError}` }
    : scanResult
      ? { matches: reviewMatches(sessionId, scanResult), total: scanResult.length }
      : undefined;

  // ── who can read it ──────────────────────────────────────────────────────
  const activeDraft = draft ?? NO_ACCESS_CHANGE;
  const access = useMemo(
    () => accessItems({ mode, publication, collectives, draft: activeDraft }),
    [activeDraft, collectives, mode, publication],
  );
  const names = useMemo(() => collectiveNames(collectives, publication), [collectives, publication]);
  const suggestions = useMemo(() => pickerSuggestions(collectives, access, query), [access, collectives, query]);
  const readerCount = access.filter((item) => item.pending !== 'removal').length;

  // The primary is on as soon as the scan is clean and someone can read the
  // transcript. An update can be sent before the collectives list arrives: its
  // draft is "no change" until the reader makes one.
  const runPublish = useCallback(async () => {
    setOutcome(null);
    setSubmitting(true);
    try {
      const response = await publish(sessionId, pushRequest(sessionId, DEFAULT_REDACTION_LEVEL, activeDraft), readerCount);
      setOutcome(publishOutcome({
        response,
        sessionId,
        names,
        audience: publication?.audience ?? [],
        fallbackUrl: publication?.transcriptUrl,
      }));
    } catch (error) {
      if (error instanceof PublishingRequestError && error.status === 401) {
        setSignIn('signed-out');
      } else {
        setOutcome({ kind: 'stopped', stoppedAt: 'sending it to village' });
      }
    } finally {
      setSubmitting(false);
    }
  }, [activeDraft, names, publication, publish, readerCount, sessionId]);

  // A step before the push that stopped, and how to retry it.
  const blocked: Blocked | null = signInFailure != null
    ? { stoppedAt: 'checking this computer’s village sign-in', retry: () => { setSignInFailure(null); setSignIn('unknown'); } }
    : collectivesRead.status === 'error'
      ? { stoppedAt: 'reading your collectives on village', retry: () => setCollectivesNonce((nonce) => nonce + 1) }
      : null;

  const dialogState: PublishDialogState = (() => {
    if (publishingTo !== undefined || submitting) return 'publishing';
    if (outcome) return outcome.kind;
    if (blocked) return 'stopped';
    if (signIn === 'waiting') return 'waiting-github';
    if (signIn === 'signed-out') return 'connect';
    if (signIn === 'unknown' || scan.phase === 'scanning') return 'checking';
    if (scan.scanError != null) return 'scan-failed';
    if (collectivesRead.status === 'ready' && collectivesRead.collectives.length === 0 && access.length === 0) return 'no-collective';
    return 'ready';
  })();

  const bar = read.status === 'ready' && read.publication
    ? (
      <PublishBar
        {...barModel({ publication: read.publication, audienceKnown: read.audienceKnown, newTurns, publishingTo })}
        onAction={openPopup}
        moreItems={moreItems}
      />
    )
    : (
      <span className="inline-flex items-center gap-3">
        {read.status === 'error' && (
          <>
            <span role="alert" className="font-mono text-[14px] text-ink-2" title={read.message}>
              the publish state could not be read
            </span>
            <button type="button" className="btn btn-ghost btn-sm" onClick={() => setReadNonce((nonce) => nonce + 1)}>
              <RotateCw size={14} aria-hidden="true" /> retry
            </button>
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
      onRemove={(id) => setDraft((current) => removeFromDraft(current ?? NO_ACCESS_CHANGE, id))}
      onRestore={(id) => setDraft((current) => restoreInDraft(current ?? NO_ACCESS_CHANGE, id))}
      picker={collectivesRead.status === 'ready'
        ? {
          suggestions,
          query,
          onQueryChange: setQuery,
          onAdd: (id) => {
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
      joinHref={`${villageUrl ?? defaultVillageUrl()}/collectives`}
      stoppedAt={outcome?.kind === 'stopped' ? outcome.stoppedAt : blocked?.stoppedAt}
      onRetry={outcome?.kind === 'stopped' ? () => { void runPublish(); } : blocked?.retry}
      done={outcome && outcome.kind !== 'stopped' ? outcome.done : undefined}
    />
  );

  return { bar, dialog };
}
