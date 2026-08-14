import { useCallback, useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import {
  RedactionStep,
  type RedactionCache,
} from '@/components/share/RedactionStep';
import {
  DEFAULT_REDACTION_LEVEL,
  type SelectableRedactionLevel,
} from '@/lib/share/redactions';
import * as redactionsApi from '@/lib/share/redactions';
import {
  REDACTION_STEP_FAILURE_EXPECTATIONS,
  REDACTION_STEP_MATCH,
  REDACTION_STEP_MOCK_EXPECTATIONS,
  REDACTION_STEP_REFRESHED_MATCH,
  REDACTION_STEP_SCAN_FAILURE,
  REDACTION_STEP_SECOND_SESSION,
  REDACTION_STEP_SESSION,
  REDACTION_STEP_STANDARD_MATCH,
} from '@/test/fixtures/redaction-step';
import { REDACTION_PREVIEW_UNKNOWN_ITEM_ERROR } from '@/test/fixtures/redaction-preview';

vi.mock('@/lib/share/redactions', async (importOriginal) => {
  const actual = await importOriginal<typeof redactionsApi>();
  return { ...actual, fetchRedactionPreview: vi.fn() };
});

const fetchPreview = vi.mocked(redactionsApi.fetchRedactionPreview);
const SESSIONS = [REDACTION_STEP_SESSION];
const SELECTED = new Set([REDACTION_STEP_SESSION.id]);
const MULTI_SESSIONS = [REDACTION_STEP_SESSION, REDACTION_STEP_SECOND_SESSION];
const MULTI_SELECTED = new Set(MULTI_SESSIONS.map((session) => session.id));

interface HarnessProps {
  multiple?: boolean;
  useMock?: boolean;
  onLevelChange?: (level: SelectableRedactionLevel) => void;
}

function Harness({ multiple = false, useMock = false, onLevelChange }: HarnessProps) {
  const [cache, setCache] = useState<RedactionCache>(() => new Map());
  const [level, setLevel] = useState<SelectableRedactionLevel>(DEFAULT_REDACTION_LEVEL);
  const [mounted, setMounted] = useState(true);
  const [footerActions, setFooterActions] = useState<import('@/components/share/footer-actions').ShareFooterActions | null>(null);
  const onNext = useCallback(() => {}, []);

  const handleLevelChange = (next: SelectableRedactionLevel) => {
    setLevel(next);
    onLevelChange?.(next);
  };

  return (
    <>
      <button type="button" onClick={() => setMounted((value) => !value)}>
        toggle redaction step
      </button>
      {mounted && (
        <RedactionStep
          sessions={multiple ? MULTI_SESSIONS : SESSIONS}
          selectedIds={multiple ? MULTI_SELECTED : SELECTED}
          redactionLevel={level}
          onLevelChange={handleLevelChange}
          onNext={onNext}
          onFooterActionsChange={setFooterActions}
          cache={cache}
          onCacheChange={setCache}
          useMock={useMock}
        />
      )}
      {footerActions?.secondary && <button type="button" onClick={footerActions.secondary.onClick}>{footerActions.secondary.label}</button>}
      {footerActions?.primary && <button type="button" onClick={footerActions.primary.onClick} disabled={footerActions.primary.disabled} title={footerActions.primary.title}>{footerActions.primary.label}</button>}
    </>
  );
}

describe('RedactionStep → RedactionReview', () => {
  afterEach(() => {
    vi.resetAllMocks();
  });

  it('scans, then renders each detected match as a before-and-after card', async () => {
    render(<Harness useMock />);

    const review = await screen.findByRole('region', { name: 'redaction review' });
    expect(
      within(review).getAllByRole('group', { name: 'before and after redaction' }).length,
    ).toBeGreaterThan(0);
    expect(within(review).getByText('all redacted')).toBeInTheDocument();
    expect(within(review).queryByRole('group', { name: 'redaction level' })).not.toBeInTheDocument();
    expect(screen.queryByText('minimal', { exact: true })).not.toBeInTheDocument();
    expect(screen.queryByText('maximum', { exact: true })).not.toBeInTheDocument();
  });

  it('reviews at the one offered level, applying each rule minimum rather than a display category', async () => {
    // The step used to walk the three-level selector to prove the mock filter
    // honours each rule's own activation minimum. Two of those levels are no
    // longer offered, so the same property is asserted at the level the product
    // actually runs: a rule that fires from minimal upward IS present, and one
    // that only fires at maximum is NOT. That is the rule-minimum semantics, and
    // it is the review a user will really see.
    render(<Harness useMock />);

    const review = await screen.findByRole('region', { name: 'redaction review' });
    expect(
      within(review).getAllByRole('group', { name: 'before and after redaction' }),
    ).toHaveLength(REDACTION_STEP_MOCK_EXPECTATIONS.standard.count);
    expect(
      within(review).getByText(REDACTION_STEP_MOCK_EXPECTATIONS.minimal.includedPath),
    ).toBeInTheDocument();
    expect(
      within(review).getByText(REDACTION_STEP_MOCK_EXPECTATIONS.standard.includedEmail),
    ).toBeInTheDocument();
    expect(
      within(review).queryByText(REDACTION_STEP_MOCK_EXPECTATIONS.maximum.includedRemote),
    ).not.toBeInTheDocument();
  });

  it('marks an explicitly kept match as content that will be sent', async () => {
    render(<Harness useMock />);
    await screen.findByRole('region', { name: 'redaction review' });

    const keepToggles = screen.getAllByRole('button', { name: 'keep' });
    expect(keepToggles.length).toBeGreaterThan(0);
    expect(screen.queryByText('will be sent')).not.toBeInTheDocument();

    await userEvent.click(keepToggles[0]);
    await waitFor(() => expect(screen.getByText('will be sent')).toBeInTheDocument());
    expect(screen.getByRole('button', { name: 'revert' })).toHaveAttribute(
      'aria-pressed',
      'true',
    );
    expect(screen.getByText(/kept un-redacted/)).toBeInTheDocument();
  });

  it('reuses a completed scan after leaving and returning to the step', async () => {
    fetchPreview.mockResolvedValue([REDACTION_STEP_MATCH]);
    render(<Harness />);

    await screen.findByRole('region', { name: 'redaction review' });
    expect(fetchPreview).toHaveBeenCalledTimes(1);

    await userEvent.click(screen.getByRole('button', { name: 'toggle redaction step' }));
    expect(screen.queryByRole('region', { name: 'redaction review' })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'toggle redaction step' }));

    await screen.findByRole('region', { name: 'redaction review' });
    expect(fetchPreview).toHaveBeenCalledTimes(1);
  });

  it('completes every selected session when progress updates the lifted cache', async () => {
    fetchPreview.mockResolvedValue([REDACTION_STEP_MATCH]);
    render(<Harness multiple />);

    await screen.findByRole('region', { name: 'redaction review' });
    expect(fetchPreview).toHaveBeenCalledTimes(2);
    expect(fetchPreview).toHaveBeenCalledWith(REDACTION_STEP_SESSION.id, DEFAULT_REDACTION_LEVEL);
    expect(fetchPreview).toHaveBeenCalledWith(
      REDACTION_STEP_SECOND_SESSION.id,
      DEFAULT_REDACTION_LEVEL,
    );
  });

  it('keeps exact scan content separate by session and restores it from cache', async () => {
    // The cache key is (level, session), and with one offered level the axis that
    // still varies - and still has to stay separate - is the session. This used to
    // walk the level selector; two of those levels are no longer offered, so it
    // walks sessions instead. The property is the same one that matters: one
    // session's findings must never be shown under another's, and a remount must
    // restore each from cache rather than rescanning.
    fetchPreview.mockImplementation((sessionID) =>
      sessionID === REDACTION_STEP_SESSION.id
        ? Promise.resolve([REDACTION_STEP_MATCH])
        : Promise.resolve([REDACTION_STEP_STANDARD_MATCH]),
    );
    render(<Harness multiple />);
    const user = userEvent.setup();

    const review = await screen.findByRole('region', { name: 'redaction review' });
    // Both sessions' exact content is present, and each came from its own scan.
    expect(within(review).getByText(REDACTION_STEP_MATCH.originalText)).toBeInTheDocument();
    expect(
      within(review).getByText(REDACTION_STEP_STANDARD_MATCH.originalText),
    ).toBeInTheDocument();
    await waitFor(() => expect(fetchPreview).toHaveBeenCalledTimes(2));
    expect(fetchPreview).toHaveBeenCalledWith(
      REDACTION_STEP_SESSION.id,
      DEFAULT_REDACTION_LEVEL,
    );
    expect(fetchPreview).toHaveBeenCalledWith(
      REDACTION_STEP_SECOND_SESSION.id,
      DEFAULT_REDACTION_LEVEL,
    );

    // A remount reads the lifted cache instead of repeating the expensive work.
    await user.click(screen.getByRole('button', { name: 'toggle redaction step' }));
    await user.click(screen.getByRole('button', { name: 'toggle redaction step' }));

    const restored = await screen.findByRole('region', { name: 'redaction review' });
    expect(within(restored).getByText(REDACTION_STEP_MATCH.originalText)).toBeInTheDocument();
    expect(
      within(restored).getByText(REDACTION_STEP_STANDARD_MATCH.originalText),
    ).toBeInTheDocument();
    expect(fetchPreview).toHaveBeenCalledTimes(2);
  });

  it('never renders an all-clear while a scan is still in flight', async () => {
    // The step has no truthful idle state, so an unsettled scan must not render as
    // a clean review. This used to reach the in-flight state by switching to an
    // unscanned level; with one offered level, Re-scan is the affordance that
    // re-opens it, and it is the one a user actually has.
    fetchPreview.mockResolvedValueOnce([REDACTION_STEP_MATCH]);
    render(<Harness />);
    await screen.findByRole('region', { name: 'redaction review' });

    fetchPreview.mockImplementationOnce(() => new Promise(() => {}));
    await userEvent.click(await screen.findByRole('button', { name: 'Re-scan' }));

    await waitFor(() =>
      expect(
        screen.queryByRole('region', { name: 'redaction review' }),
      ).not.toBeInTheDocument(),
    );
    expect(
      screen.queryByText(REDACTION_STEP_FAILURE_EXPECTATIONS.forbiddenSafeCopy),
    ).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Continue' })).toBeDisabled();
  });

  it('replaces the current cache entry with exact Re-scan content', async () => {
    fetchPreview
      .mockResolvedValueOnce([REDACTION_STEP_MATCH])
      .mockResolvedValueOnce([REDACTION_STEP_REFRESHED_MATCH]);
    render(<Harness />);

    const review = await screen.findByRole('region', { name: 'redaction review' });
    expect(within(review).getByText(REDACTION_STEP_MATCH.originalText)).toBeInTheDocument();
    expect(fetchPreview).toHaveBeenCalledTimes(1);

    await userEvent.click(await screen.findByRole('button', { name: 'Re-scan' }));
    expect(
      await screen.findByText(REDACTION_STEP_REFRESHED_MATCH.originalText),
    ).toBeInTheDocument();
    const refreshedReview = await screen.findByRole('region', { name: 'redaction review' });
    expect(
      within(refreshedReview).getByText(REDACTION_STEP_REFRESHED_MATCH.redactedReplacement),
    ).toBeInTheDocument();
    expect(
      within(refreshedReview).queryByText(REDACTION_STEP_MATCH.originalText),
    ).not.toBeInTheDocument();
    expect(fetchPreview).toHaveBeenCalledTimes(2);
    expect(fetchPreview).toHaveBeenNthCalledWith(
      1,
      REDACTION_STEP_SESSION.id,
      DEFAULT_REDACTION_LEVEL,
    );
    expect(fetchPreview).toHaveBeenNthCalledWith(
      2,
      REDACTION_STEP_SESSION.id,
      DEFAULT_REDACTION_LEVEL,
    );
  });

  it('preserves an honest scan failure after leaving and returning', async () => {
    fetchPreview.mockRejectedValue(new Error(REDACTION_STEP_SCAN_FAILURE));
    render(<Harness />);

    expect(await screen.findByRole('alert')).toHaveTextContent(REDACTION_STEP_SCAN_FAILURE);
    expect(
      screen.getByRole('progressbar', {
        name: REDACTION_STEP_FAILURE_EXPECTATIONS.allFailure.scannedLabel,
      }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(REDACTION_STEP_FAILURE_EXPECTATIONS.allFailure.progressText),
    ).toBeInTheDocument();
    expect(
      screen.getByText(REDACTION_STEP_FAILURE_EXPECTATIONS.allFailure.incompleteTitle),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(REDACTION_STEP_FAILURE_EXPECTATIONS.forbiddenSafeCopy),
    ).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Continue' })).toBeDisabled();
    expect(fetchPreview).toHaveBeenCalledTimes(1);

    await userEvent.click(screen.getByRole('button', { name: 'toggle redaction step' }));
    await userEvent.click(screen.getByRole('button', { name: 'toggle redaction step' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(REDACTION_STEP_SCAN_FAILURE);
    expect(
      screen.getByRole('progressbar', {
        name: REDACTION_STEP_FAILURE_EXPECTATIONS.allFailure.scannedLabel,
      }),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(REDACTION_STEP_FAILURE_EXPECTATIONS.forbiddenSafeCopy),
    ).not.toBeInTheDocument();
    expect(fetchPreview).toHaveBeenCalledTimes(1);
  });

  it('caches an unknown-item category failure and keeps Continue disabled', async () => {
    fetchPreview.mockRejectedValue(new Error(REDACTION_PREVIEW_UNKNOWN_ITEM_ERROR));
    render(<Harness />);

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'redaction preview category "LOCATION" is not recognized',
    );
    expect(screen.getByRole('button', { name: 'Continue' })).toBeDisabled();
    expect(fetchPreview).toHaveBeenCalledTimes(1);

    await userEvent.click(screen.getByRole('button', { name: 'toggle redaction step' }));
    await userEvent.click(screen.getByRole('button', { name: 'toggle redaction step' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'the preview cannot safely classify this finding, so sharing remains blocked',
    );
    expect(screen.getByRole('button', { name: 'Continue' })).toBeDisabled();
    expect(fetchPreview).toHaveBeenCalledTimes(1);
  });

  it('recovers from a failed scan by re-scanning rather than by weakening the level', async () => {
    // Recovery used to mean switching down a level, which is why the failure
    // bridge rendered a level picker at all. That is no longer a recovery path:
    // the weaker levels are refused, and "recover by protecting less" was never a
    // good answer to a transient scan failure. Re-scan at the offered level is,
    // and the honest failure must persist until it succeeds.
    const { recovery } = REDACTION_STEP_FAILURE_EXPECTATIONS;
    fetchPreview.mockRejectedValueOnce(new Error(REDACTION_STEP_SCAN_FAILURE));
    render(<Harness />);

    expect(await screen.findByRole('alert')).toHaveTextContent(REDACTION_STEP_SCAN_FAILURE);
    expect(
      screen.queryByText(REDACTION_STEP_FAILURE_EXPECTATIONS.forbiddenSafeCopy),
    ).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Continue' })).toBeDisabled();

    fetchPreview.mockResolvedValueOnce([REDACTION_STEP_STANDARD_MATCH]);
    await userEvent.click(await screen.findByRole('button', { name: recovery.recoveryLevel }));

    const review = await screen.findByRole('region', { name: 'redaction review' });
    expect(
      within(review).getByText(REDACTION_STEP_STANDARD_MATCH.originalText),
    ).toBeInTheDocument();
    expect(
      within(review).getByText(REDACTION_STEP_STANDARD_MATCH.redactedReplacement),
    ).toBeInTheDocument();
    // Every scan ran at the offered level: recovery never reached for a weaker one.
    expect(fetchPreview).toHaveBeenCalledTimes(recovery.calls.length);
    for (const call of fetchPreview.mock.calls) {
      expect(call[1]).toBe(DEFAULT_REDACTION_LEVEL);
    }
  });

  it('keeps mixed success and failure truthful across remounts', async () => {
    fetchPreview.mockImplementation((sessionID) =>
      sessionID === REDACTION_STEP_SESSION.id
        ? Promise.resolve([REDACTION_STEP_MATCH])
        : Promise.reject(new Error(REDACTION_STEP_SCAN_FAILURE)),
    );
    render(<Harness multiple />);

    let review = await screen.findByRole('region', { name: 'redaction review' });
    expect(within(review).getByText(REDACTION_STEP_MATCH.originalText)).toBeInTheDocument();
    expect(within(review).getByRole('alert')).toHaveTextContent(REDACTION_STEP_SCAN_FAILURE);
    expect(
      within(review).getByRole('progressbar', {
        name: REDACTION_STEP_FAILURE_EXPECTATIONS.mixed.scannedLabel,
      }),
    ).toBeInTheDocument();
    expect(
      within(review).getByText(REDACTION_STEP_FAILURE_EXPECTATIONS.mixed.progressText),
    ).toBeInTheDocument();
    expect(
      within(review).queryByText(REDACTION_STEP_FAILURE_EXPECTATIONS.forbiddenSafeCopy),
    ).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Continue' })).toBeDisabled();
    expect(fetchPreview).toHaveBeenCalledTimes(2);

    await userEvent.click(screen.getByRole('button', { name: 'toggle redaction step' }));
    await userEvent.click(screen.getByRole('button', { name: 'toggle redaction step' }));

    review = await screen.findByRole('region', { name: 'redaction review' });
    expect(within(review).getByText(REDACTION_STEP_MATCH.originalText)).toBeInTheDocument();
    expect(within(review).getByRole('alert')).toHaveTextContent(REDACTION_STEP_SCAN_FAILURE);
    expect(
      within(review).getByRole('progressbar', {
        name: REDACTION_STEP_FAILURE_EXPECTATIONS.mixed.scannedLabel,
      }),
    ).toBeInTheDocument();
    expect(fetchPreview).toHaveBeenCalledTimes(2);
  });
});
