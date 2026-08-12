'use client';

import { useCallback, useState, useEffect, useMemo } from 'react';
import Link from 'next/link';
import { useSearchParams } from 'next/navigation';
import { Breadcrumbs } from '@/components/Breadcrumbs';
import {
  Button,
  StepIndicator,
  type WizardStep as FtWizardStep,
} from '@/lib/ft-ui';
import { SessionPicker } from '@/components/share/SessionPicker';
import { LabelsStep } from '@/components/share/LabelsStep';
import {
  RedactionStep,
  type RedactionCache,
} from '@/components/share/RedactionStep';
import { PushStep } from '@/components/share/PushStep';
import type { ShareDiscoveryResult, ShareSession, ShareHierarchySession, LabelSelection } from '@/lib/share/types';
import { emptyLabelSelection } from '@/lib/share/types';
import {
  DEFAULT_REDACTION_LEVEL,
  type SelectableRedactionLevel,
} from '@/lib/share/redactions';
import { groupByProject, isSelectable } from '@/lib/share/group';
import { fetchMockSessions } from '@/lib/share/mock-data';
import { useMockConfig } from '@/hooks/useMockConfig';
import { getApiBaseUrl } from '@/lib/api/base';
import { fetchDiscovery, requireDiscoveryItem } from '@/lib/api/discovery';

// Prior-version Contribute wizard: superseded by the fairtrade graph shell lift,
// a deprecation candidate retained for evidence exits until its replacement lands.

interface BackendSessionSummary {
  id: string;
  harness: string;
  startTime: string;
  durationMins: number;
  totalTokens: number;
  turnCount: number;
  toolCallCount: number;
  project?: string;
  /** Redaction-safe first user message — see SessionSummary.preview (Go). */
  preview?: string;
  /** Heuristic outcome — see SessionSummary.outcome (Go). */
  outcome?: string;
  shareStatus?: ShareSession['shareStatus'];
}

function mapBackendToShareSession(backend: BackendSessionSummary): ShareSession {
  return {
    id: backend.id,
    provider: backend.harness as ShareSession['provider'],
    projectName: backend.project ?? 'Unknown Project',
    projectHash: '',
    hostSlug: '',
    startTime: backend.startTime,
    durationMins: Math.round(backend.durationMins),
    totalTokens: backend.totalTokens,
    turnCount: backend.turnCount,
    model: '',
    shareStatus: backend.shareStatus ?? 'new',
    preview: backend.preview ?? '',
    outcome: backend.outcome,
  };
}

function countByStatus(sessions: ShareSession[]): Record<ShareSession['shareStatus'], number> {
  const counts: Record<ShareSession['shareStatus'], number> = { new: 0, updated: 0, shared: 0, held: 0, error: 0, pushing: 0 };
  for (const s of sessions) counts[s.shareStatus]++;
  return counts;
}

async function fetchRealSessions(): Promise<ShareDiscoveryResult<ShareHierarchySession>> {
  const [response, metadata] = await Promise.all([
    fetch(`${getApiBaseUrl()}/api/v1/sessions`),
    fetchDiscovery(),
  ]);
  if (!response.ok) {
    let detail = `the server returned HTTP ${response.status} without an actionable response body`;
    try {
      const payload = await response.json() as { error?: string };
      if (payload.error) detail = payload.error;
    } catch {
      const body = await response.text().catch(() => '');
      if (body.trim()) detail = body.trim();
    }
    throw new Error(`Session discovery failed while loading the Share chooser: ${detail}`);
  }
  const payload = await response.json();
  const summaries: BackendSessionSummary[] = payload.sessions ?? [];
  const sessions: ShareHierarchySession[] = summaries.map((summary) => {
    const session = mapBackendToShareSession(summary);
    const item = requireDiscoveryItem(metadata, session.id, 'mounted Share chooser');
    return { ...session, locationLabel: item.locationLabel, repositoryLocationId: item.repositoryLocationId, branch: item.branch };
  });
  return {
    sessions,
    counts: countByStatus(sessions),
  };
}

// Local step-id union — the four visible wizard steps.
type WizardStep = 'select' | 'labels' | 'redact' | 'submit';

// Step descriptors for the fairtrade StepIndicator rail.
// Title-case labels render lowercase in the browser via CSS (swz-label text-transform).
const WIZARD_STEPS: FtWizardStep[] = [
  { id: 'select', label: 'choose' },
  { id: 'labels', label: 'labels' },
  { id: 'redact', label: 'redact' },
  { id: 'submit', label: 'submit' },
];

// Deep-link contract. Four visible steps; legacy step names keep working by
// mapping onto the current ones.
const STEP_ALIASES: Record<string, WizardStep> = {
  select: 'select',
  // The old forced "annotations" step is now the optional Labels step.
  annotations: 'labels',
  labels: 'labels',
  redact: 'redact',
  // "push"/"contribute" both meant the final step — now Submit.
  push: 'submit',
  contribute: 'submit',
  submit: 'submit',
};

function resolveDeepLinkStep(raw: string | null): WizardStep | null {
  if (raw === null) return null;
  return STEP_ALIASES[raw] ?? null;
}

// Linear advance order for the visible step machine.
const STEP_ORDER: WizardStep[] = ['select', 'labels', 'redact', 'submit'];

export function ShareWizardClient() {
  const { config, loading, error: configError } = useMockConfig();
  const searchParams = useSearchParams();

  // Deep-link contract: /share?sessionId={id}&step={select|redact|push|contribute}
  // Legacy step names (annotations, redact, push) still resolve. Both params
  // are optional and silently ignored when invalid.
  const deepLinkSessionId = searchParams?.get('sessionId') ?? null;
  const deepLinkStep = resolveDeepLinkStep(searchParams?.get('step') ?? null);

  // Evidence-set entry: /share?sessions=<id,id,…> arrives from
  // "Contribute sessions →" in Review or from a Map node. The Choose
  // list is FILTERED to those sessions, with a one-click "Select these N
  // sessions" affordance — filtered, NOT preselected, so the opt-in posture
  // holds. Cold entry (no param) is unchanged: everything listed, nothing
  // selected.
  const evidenceParam = searchParams?.get('sessions') ?? null;
  const evidenceIds = useMemo<Set<string> | null>(() => {
    if (!evidenceParam) return null;
    const ids = evidenceParam
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean);
    return ids.length > 0 ? new Set(ids) : null;
  }, [evidenceParam]);

  // Wizard navigation — visible steps: Choose → Labels → Redact → Submit.
  const [step, setStep] = useState<WizardStep>('select');

  // Track explicitly completed steps for the rail's olive+check markers.
  // A step enters this set when the user clicks Continue in its body (goNext).
  // Deep-linking into a mid-flow step does not retroactively mark earlier
  // steps complete — the user hasn't reviewed them.
  const [completed, setCompleted] = useState<Set<WizardStep>>(new Set());

  // Redaction is safe-by-default (everything flagged is redacted unless the
  // user opts an item out), so there is no explicit approval gate anymore.
  // Submit is reachable as soon as a non-empty selection exists.

  // Labels chosen on the (optional) Labels step. These are real annotations
  // (GET /api/v1/annotations) grouped auto/manual; the included ids flow into
  // the push selection (`peasant push --annotation-id`). Carried in wizard
  // state and surfaced in the Submit transparency panel.
  const [labels, setLabels] = useState<LabelSelection>(() => emptyLabelSelection());

  // Config-aware data fetching
  const [discovery, setDiscovery] = useState<ShareDiscoveryResult<ShareHierarchySession> | null>(null);
  const [fetchError, setFetchError] = useState<string | null>(null);
  const [retryCount, setRetryCount] = useState(0);

  // Whether session data (and, by extension, labels) should come from mock.
  // Mirrors the same toggle used for session discovery so the Labels step reads
  // mock annotations when the rest of the wizard is mocked.
  const useMock = useMemo(
    () => !!config && config.enabled && !!config.web?.includes('sessions'),
    [config],
  );

  useEffect(() => {
    if (loading) return;

    if (configError) {
      setDiscovery(null);
      setSelectedIds(new Set());
      setFetchError(configError);
      return;
    }

    if (config) {
      if (useMock) {
        setFetchError(null);
        const result = fetchMockSessions();
        setDiscovery({ ...result, sessions: result.sessions.map((session) => ({ ...session, locationLabel: 'mock repository', repositoryLocationId: `mock:${session.projectHash || session.projectName}`, branch: 'main' })) });
      } else {
        fetchRealSessions()
          .then((result) => {
            setFetchError(null);
            setDiscovery(result);
          })
          .catch((err) => {
            setDiscovery(null);
            setSelectedIds(new Set());
            setFetchError(err instanceof Error ? err.message : 'Failed to fetch sessions');
          });
      }
    }
  }, [config, loading, configError, retryCount, useMock]);

  // Selection is session-ids (Labels/Redaction/Push keep their props). The
  // Choose starts empty so the user opts in. A ?sessionId= deep-link
  // is the only thing that preselects, and only that one session.
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());

  const selectableIds = useMemo(
    () => new Set(discovery?.sessions.filter(isSelectable).map((session) => session.id) ?? []),
    [discovery],
  );

  useEffect(() => {
    if (!discovery) return;

    const linked =
      deepLinkSessionId &&
      discovery.sessions.find((s) => s.id === deepLinkSessionId);

    if (linked && selectableIds.has(linked.id)) {
      // The deep-link is a drill-in: select exactly that one session, not its
      // whole project.
      const project = groupByProject(discovery.sessions).find((p) =>
        p.sessions.some((s) => s.id === linked.id),
      );
      void project; // grouping confirms the project exists; selection is the single session
      setSelectedIds(new Set([linked.id]));
    } else {
      // No deep-link → nothing selected. The user picks on the Choose step.
      setSelectedIds(new Set());
    }

    if (deepLinkStep) {
      setStep(deepLinkStep);
    }
  }, [discovery, deepLinkSessionId, deepLinkStep, selectableIds]);

  // Redaction level. It starts - and stays - at the single level this version
  // offers.
  //
  // It used to start at 'maximum', described here as stripping every detected
  // pattern. Two things were wrong with that. The claim was a completeness claim
  // pattern matching cannot deliver, and the level itself is one the local API now
  // refuses outright, so the wizard opened at a setting whose very first scan
  // request answered 400. Both endpoints reject it, so this was not a preference
  // to dial down but a dead end to walk into.
  const [redactionLevel, setRedactionLevel] =
    useState<SelectableRedactionLevel>(DEFAULT_REDACTION_LEVEL);

  // Cache completed scans for the lifetime of this wizard. Entries include
  // failures so revisiting Redact cannot turn an honest warning into all-clear.
  const [redactionCache, setRedactionCache] = useState<RedactionCache>(
    () => new Map(),
  );

  // Advance to the next step and mark the current step as complete in the
  // rail (olive + check). Each step body calls this via its own Continue button.
  const goNext = useCallback(() => {
    setCompleted((prev) => {
      const next = new Set(prev);
      next.add(step);
      return next;
    });
    setStep((prev) => {
      const i = STEP_ORDER.indexOf(prev);
      return i >= 0 && i < STEP_ORDER.length - 1 ? STEP_ORDER[i + 1] : prev;
    });
  }, [step]);

  // Navigate back one step. Completed markers are preserved so the rail still
  // shows the checkmark for the step we've returned from.
  const goBack = useCallback(() => {
    setStep((prev) => {
      const i = STEP_ORDER.indexOf(prev);
      return i > 0 ? STEP_ORDER[i - 1] : prev;
    });
  }, []);

  // Which steps the StepIndicator allows jumping to. Choose and Labels are
  // always reachable; Redact and Submit need a non-empty selection. Redaction
  // is safe-by-default, so Submit is no longer gated behind an approval.
  const reachable = useMemo<Set<WizardStep>>(() => {
    const r = new Set<WizardStep>(['select', 'labels']);
    if (selectedIds.size > 0) {
      r.add('redact');
      r.add('submit');
    }
    return r;
  }, [selectedIds]);

  // If the user lands on Submit with nothing selected (e.g. a stale
  // deep-link), bounce them back to Choose. There is no redaction gate.
  useEffect(() => {
    if (step === 'submit' && selectedIds.size === 0) {
      setStep('select');
    }
  }, [step, selectedIds]);

  const stepIndex = STEP_ORDER.indexOf(step);

  // Show loading while config is being fetched
  if (loading || (!discovery && !fetchError)) {
    return (
      <div className="max-w-6xl mx-auto px-6 pt-6 pb-12">
        <div className="flex items-center justify-center py-20">
          <p className="text-sm text-ink-3">Loading sessions...</p>
        </div>
      </div>
    );
  }

  // Show error UI when fetch failed (no mock fallback)
  if (fetchError) {
    return (
      <div className="max-w-6xl mx-auto px-6 pt-6 pb-12">
        <div className="flex flex-col items-center justify-center py-20 gap-4">
          <p className="text-[13px] text-danger px-3 py-2 border border-danger/30 bg-danger-soft">
            {fetchError}
          </p>
          <button
            type="button"
            onClick={() => {
              setFetchError(null);
              setRetryCount((c) => c + 1);
            }}
            className="text-sm underline text-ink-3 hover:text-ink transition-colors focus-mono cursor-pointer"
          >
            Retry
          </button>
        </div>
      </div>
    );
  }

  // discovery is guaranteed non-null past the guards above.
  const disc = discovery!;

  // The Choose list, filtered to the evidence set when one rode in on the
  // URL. Labels/Redact/Submit keep the full list — the selection (which only
  // ever contains visible Choose rows) is what scopes them.
  const evidenceSessions = evidenceIds
    ? disc.sessions.filter((s) => evidenceIds.has(s.id))
    : null;
  const chooseSessions = evidenceSessions ?? disc.sessions;

  return (
    <div className="max-w-[1600px] mx-auto px-6 pt-6 pb-12 flex flex-col gap-6 animate-fade-up">
      <Breadcrumbs items={[{ label: 'contribute' }]} />

      {/* Page title block. */}
      <div>
        <h1 className="font-[family-name:var(--font-display)] text-2xl font-semibold tracking-tight text-ink">
          Contribute
        </h1>
      </div>

      {/* Wizard shell: step rail + body + back/counter footer.
          Uses the fairtrade swz layout classes (via @peasant-labs/fairtrade/components.css
          already imported in layout.tsx). Forward navigation lives inside each step
          body's own Continue/Submit button; the footer owns back + step count. */}
      <section className="swz" aria-label="contribute to the commons" data-tour="share-nav">

        {/* Step rail — completed = olive+check, current = amber, locked = dim/disabled. */}
        <div className="swz-head">
          <StepIndicator
            steps={WIZARD_STEPS}
            current={step}
            completed={completed as Set<string>}
            reachable={reachable as Set<string>}
            onJump={(id) => setStep(id as WizardStep)}
            aria-label="Contribute progress"
          />
        </div>

        {/* Per-step body. The kicker names the active step in mono chrome above the
            content; the content components own their own internal layout. */}
        <div className="swz-body">
          <span className="swz-body-kicker">
            step {stepIndex + 1}: {WIZARD_STEPS[stepIndex]?.label}
          </span>

          {/* Evidence-set strip — only when ?sessions= filtered the Choose list.
              One click selects the whole set; nothing is preselected. */}
          {step === 'select' && evidenceSessions && (
            <div className="border border-rule bg-surface px-4 py-3 flex flex-wrap items-center justify-between gap-3 mb-4">
              <p className="text-[13px] text-ink-3">
                {evidenceSessions.length > 0 ? (
                  <>
                    Showing only the{' '}
                    <span className="font-mono tabular-nums">{evidenceSessions.length}</span>{' '}
                    session{evidenceSessions.length !== 1 ? 's' : ''} behind this change.{' '}
                  </>
                ) : (
                  <>None of the linked sessions were found on this machine. </>
                )}
                <Link
                  href="/share"
                  className="underline underline-offset-2 hover:text-ink transition-colors focus-mono"
                >
                  Show all sessions
                </Link>
              </p>
              {evidenceSessions.length > 0 && (
                <Button
                  size="sm"
                  variant="secondary"
                  onClick={() => setSelectedIds(new Set(evidenceSessions.map((s) => s.id).filter((id) => selectableIds.has(id))))}
                >
                  {evidenceSessions.length === 1
                    ? 'Select this session'
                    : `Select these ${evidenceSessions.length} sessions`}
                </Button>
              )}
            </div>
          )}

          {/* Step 1 — Choose (projects). Starts empty + select-all. */}
          {step === 'select' && (
            <SessionPicker
              sessions={chooseSessions}
              selectedIds={selectedIds}
              onSelectionChange={setSelectedIds}
              onNext={goNext}
            />
          )}

          {/* Step 2 — Labels (optional, skippable). */}
          {step === 'labels' && (
            <LabelsStep
              sessions={disc.sessions}
              selectedIds={selectedIds}
              onLabelsChange={setLabels}
              onNext={goNext}
              useMock={useMock}
            />
          )}

          {/* Step 3 — Redact. Safe-by-default: everything flagged is redacted
              unless the user opts an item out. No gate — onNext just advances. */}
          {step === 'redact' && (
            <RedactionStep
              sessions={disc.sessions}
              selectedIds={selectedIds}
              redactionLevel={redactionLevel}
              onLevelChange={setRedactionLevel}
              onNext={goNext}
              cache={redactionCache}
              onCacheChange={setRedactionCache}
              useMock={useMock}
            />
          )}

          {/* Step 4 — Submit. Reachable once a selection exists; redaction is
              safe-by-default so there is no approval gate. */}
          {step === 'submit' && selectedIds.size > 0 && (
            <PushStep
              sessions={disc.sessions}
              selectedIds={selectedIds}
              labels={labels}
              redactionLevel={redactionLevel}
              useMock={useMock}
            />
          )}
        </div>

        {/* Sticky footer: back on the left, step counter centred.
            The right slot is empty — each step body's own Continue/Submit button
            handles forward navigation so users see contextual labels ("Skip",
            "Continue", "Contribute") rather than a generic "continue" here. */}
        <div className="swz-foot">
          <Button
            variant="ghost"
            onClick={goBack}
            disabled={stepIndex === 0}
          >
            back
          </Button>
          <span className="swz-count" aria-hidden="true">
            step <span className="tnum">{stepIndex + 1}</span>{' '}
            /{' '}
            <span className="tnum">{STEP_ORDER.length}</span>
          </span>
          {/* right slot: intentionally empty — forward nav is per-step */}
          <span aria-hidden="true" />
        </div>

      </section>
    </div>
  );
}
