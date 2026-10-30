'use client';

import { useEffect, useId, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { Tag, Loader2, Check, ChevronLeft } from 'lucide-react';
import { TranscriptLabelPopover, type SavedLabel } from '@peasant-labs/fairtrade/ui';
import { cn } from '@/lib/utils';
import './TurnLabelPopover.css';
import {
  entryApplicableTypes,
  permissibleValues,
  saveTurnLabel,
  saveTurnLabels,
  TURN_OUTCOME_TYPE_ID,
  TURN_FLAG_TYPE_ID,
  type AnnotationType,
} from '@/lib/api/annotations';

export interface SavedTurnLabel {
  /** The entry index this label targets (turn.index). */
  entryIndex: number;
  typeId: string;
  typeName: string;
  value: string;
  /** DB id returned by the batch endpoint; empty for an optimistic pre-save. */
  id: string;
}

interface TurnLabelPopoverProps {
  sessionId: string;
  /** The turn's entry index (turn.index) — used as targetEntryIndex. */
  entryIndex: number;
  /** All annotation types fetched from the backend (unfiltered). */
  types: AnnotationType[];
  /** This turn's already-saved labels (any type), used to prefill the outcome
   *  + flag modal and to offer "already set" values in the more-labels list. */
  savedLabels?: SavedTurnLabel[];
  /** Called with each saved label after a successful POST so the parent can
   *  merge it into its annotation map (optimistic render). */
  onSaved: (label: SavedTurnLabel) => void;
  className?: string;
}

/**
 * The fairtrade demo's flag vocabulary is the fidelity oracle and is
 * fixed inside `TranscriptLabelPopover` ('' = none, 'retry-loop' = hyphenated)
 * and is not configurable from the host. Peasant persists flag values as
 * enumerated `quality.turn_flag` annotation text ('none', 'retry_loop' —
 * underscored, matching every other seeded enum in this registry, e.g.
 * quality.frustration_signal's 'not_detected'). These two maps are the only
 * translation boundary between the DS component's local vocabulary and the
 * persisted one; every other flag value is identical in both.
 */
const FLAG_UI_TO_STORED: Record<string, string> = {
  '': 'none',
  'retry-loop': 'retry_loop',
};
const FLAG_STORED_TO_UI: Record<string, string> = {
  none: '',
  retry_loop: 'retry-loop',
};
function flagUiToStored(uiFlag: string): string {
  return FLAG_UI_TO_STORED[uiFlag] ?? uiFlag;
}
function flagStoredToUi(storedFlag: string): string {
  return FLAG_STORED_TO_UI[storedFlag] ?? storedFlag;
}

/**
 * Per-turn labeling affordance: the restored outcome+flag modal composing the design
 * system's canonical `TranscriptLabelPopover`, plus a secondary "more labels"
 * picker preserving the typed annotation-registry affordances
 * (`user.custom_label` free text, `quality.frustration_signal` /
 * `quality.resolution_evidence` manual apply) that the outcome+flag modal's
 * fixed shape has no room for. Both affordances persist through the same
 * `/api/v1/annotations/batch` endpoint — there is no local-only/fake state.
 */
export function TurnLabelPopover({
  sessionId,
  entryIndex,
  types,
  savedLabels,
  onSaved,
  className,
}: TurnLabelPopoverProps) {
  return (
    <span className="inline-flex items-center gap-1">
      <OutcomeFlagPopover
        sessionId={sessionId}
        entryIndex={entryIndex}
        savedLabels={savedLabels}
        onSaved={onSaved}
        className={className}
      />
      <MoreLabelsPopover
        sessionId={sessionId}
        entryIndex={entryIndex}
        types={types}
        onSaved={onSaved}
        className={className}
      />
    </span>
  );
}

// ---------------------------------------------------------------------------
// Primary affordance: the restored outcome+flag modal.
// ---------------------------------------------------------------------------

interface OutcomeFlagPopoverProps {
  sessionId: string;
  entryIndex: number;
  savedLabels?: SavedTurnLabel[];
  onSaved: (label: SavedTurnLabel) => void;
  className?: string;
}

function OutcomeFlagPopover({
  sessionId,
  entryIndex,
  savedLabels,
  onSaved,
  className,
}: OutcomeFlagPopoverProps) {
  const [open, setOpen] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);

  const currentOutcome = savedLabels?.find((l) => l.typeId === TURN_OUTCOME_TYPE_ID)?.value;
  const currentFlagStored = savedLabels?.find((l) => l.typeId === TURN_FLAG_TYPE_ID)?.value;
  const current: SavedLabel | undefined = currentOutcome
    ? {
        outcome: currentOutcome as SavedLabel['outcome'],
        flag: currentFlagStored != null ? flagStoredToUi(currentFlagStored) : undefined,
      }
    : undefined;

  async function handleSave(outcome: SavedLabel['outcome'], flag: string) {
    setSaving(true);
    setError(null);
    try {
      const ids = await saveTurnLabels({
        sessionId,
        entryIndex,
        items: [
          { typeId: TURN_OUTCOME_TYPE_ID, value: outcome },
          { typeId: TURN_FLAG_TYPE_ID, value: flagUiToStored(flag) },
        ],
      });
      onSaved({
        entryIndex,
        typeId: TURN_OUTCOME_TYPE_ID,
        typeName: 'Turn outcome',
        value: outcome,
        id: ids[0],
      });
      onSaved({
        entryIndex,
        typeId: TURN_FLAG_TYPE_ID,
        typeName: 'Turn flag',
        value: flagUiToStored(flag),
        id: ids[1],
      });
      // Close on success, matching the DS mockup's save-then-dismiss flow.
      setOpen(false);
      triggerRef.current?.focus();
    } catch (e) {
      // `TranscriptLabelPopover` has no error slot of its own (it is a dumb,
      // host-agnostic component); keep the trigger's own popover CLOSED but
      // surface an actionable, retryable banner beside it rather than
      // silently swallowing the failure.
      setError(
        e instanceof Error
          ? e.message
          : `Saving the turn ${entryIndex} outcome/flag label failed with a non-Error rejection from saveTurnLabels in TurnLabelPopover.handleSave.`,
      );
      setOpen(false);
      triggerRef.current?.focus();
    } finally {
      setSaving(false);
    }
  }

  return (
    <span className="inline-flex items-center">
      <button
        ref={triggerRef}
        type="button"
        aria-label="Label this turn"
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={() => {
          setError(null);
          setOpen((o) => !o);
        }}
        className={cn(
          'btn btn-ghost btn-sm peasant-turn-label-trigger focus-mono',
          className,
        )}
      >
        <Tag aria-hidden="true" />
        label
      </button>
      {/* `TranscriptLabelPopover`'s `.txn-label-scrim` is `position:absolute;
          inset:0` — the design system's own canonical usage mounts it as a
          direct child of the composite's full-viewport root, NOT inside a
          small `position:relative` trigger anchor (which would clip/misplace
          the scrim to the trigger's own tiny box). Portal to `document.body`
          so it resolves against the viewport here too, matching the oracle's
          centered-dialog fidelity. */}
      {open && typeof document !== 'undefined' && createPortal(
        <TranscriptLabelPopover
          turnId={entryIndex}
          current={current}
          onSave={(outcome, flag) => {
            if (saving) return;
            void handleSave(outcome, flag);
          }}
          onClose={() => {
            setOpen(false);
            triggerRef.current?.focus();
          }}
        />,
        document.body,
      )}
      {!open && error && (
        <span role="alert" className="peasant-turn-label-error">
          {error}{' '}
          <button
            type="button"
            className="btn btn-ghost btn-sm focus-mono"
            onClick={() => {
              setError(null);
              setOpen(true);
            }}
          >
            retry
          </button>
        </span>
      )}
    </span>
  );
}

// ---------------------------------------------------------------------------
// Secondary affordance: the typed annotation-registry picker (free text +
// enumerated system/user types other than outcome/flag), preserved from the
// pre-restoration implementation so custom free-text labels and manual
// application of the system classifiers are not lost.
// ---------------------------------------------------------------------------

interface MoreLabelsPopoverProps {
  sessionId: string;
  entryIndex: number;
  types: AnnotationType[];
  onSaved: (label: SavedTurnLabel) => void;
  className?: string;
}

function MoreLabelsPopover({
  sessionId,
  entryIndex,
  types,
  onSaved,
  className,
}: MoreLabelsPopoverProps) {
  const [open, setOpen] = useState(false);
  const [activeType, setActiveType] = useState<AnnotationType | null>(null);
  const [freeText, setFreeText] = useState('');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const freeTextId = useId();
  const popId = useId();
  const anchorRef = useRef<HTMLSpanElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);

  // The outcome/flag types have their own dedicated modal (OutcomeFlagPopover)
  // — omit them here so the same type never appears in both pickers.
  const applicable = useMemo(
    () =>
      entryApplicableTypes(types).filter(
        (t) => t.typeId !== TURN_OUTCOME_TYPE_ID && t.typeId !== TURN_FLAG_TYPE_ID,
      ),
    [types],
  );

  function reset() {
    setActiveType(null);
    setFreeText('');
    setSaving(false);
    setError(null);
  }

  useEffect(() => {
    if (!open) return;
    const onDocDown = (e: MouseEvent) => {
      if (anchorRef.current && !anchorRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        setOpen(false);
        triggerRef.current?.focus();
      }
    };
    document.addEventListener('mousedown', onDocDown);
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('mousedown', onDocDown);
      document.removeEventListener('keydown', onKey);
    };
  }, [open]);

  useEffect(() => {
    if (!open) reset();
  }, [open]);

  async function confirm(type: AnnotationType, value: string) {
    setSaving(true);
    setError(null);
    try {
      const id = await saveTurnLabel({
        sessionId,
        entryIndex,
        typeId: type.typeId,
        value,
      });
      onSaved({
        entryIndex,
        typeId: type.typeId,
        typeName: type.displayName,
        value,
        id,
      });
      setOpen(false);
      triggerRef.current?.focus();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to save label');
      setSaving(false);
    }
  }

  if (applicable.length === 0) return null;

  return (
    <span className="tip-anchor" ref={anchorRef}>
      <button
        ref={triggerRef}
        type="button"
        aria-label="More labels"
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-controls={open ? popId : undefined}
        onClick={() => setOpen((o) => !o)}
        className={cn(
          'btn btn-ghost btn-sm peasant-turn-label-trigger focus-mono',
          className,
        )}
      >
        <Tag aria-hidden="true" />
        more
      </button>
      {open && (
        <div
          className="pop-card peasant-turn-label-popover"
          role="dialog"
          aria-label="More labels"
          id={popId}
        >
          {activeType == null ? (
            <div className="flex flex-col">
              <p className="px-3 pt-2.5 pb-1.5 text-[length:var(--fs-label)] font-mono text-ink-4">
                more labels
              </p>
              {applicable.map((t) => (
                <button
                  key={t.typeId}
                  type="button"
                  onClick={() => setActiveType(t)}
                  className="flex flex-col gap-0.5 px-3 py-2 text-left hover:bg-surface-hover focus-mono transition-colors"
                >
                  <span className="text-[length:var(--fs-body)] text-ink">{t.displayName}</span>
                  {t.description && (
                    <span className="text-[length:var(--fs-body)] text-ink-3 line-clamp-2">
                      {t.description}
                    </span>
                  )}
                </button>
              ))}
            </div>
          ) : (
            <div className="flex flex-col">
              <div className="flex items-center gap-2 px-3 pt-2.5 pb-1.5">
                <button
                  type="button"
                  onClick={() => setActiveType(null)}
                  disabled={saving}
                  className="inline-flex items-center gap-[var(--sp-1)] text-[length:var(--fs-label)] font-mono text-ink-4 hover:text-ink focus-mono disabled:opacity-50"
                >
                  <ChevronLeft aria-hidden="true" className="size-[var(--ic-sm)]" />
                  back
                </button>
                <span className="text-[length:var(--fs-body)] text-ink font-medium truncate">
                  {activeType.displayName}
                </span>
              </div>
              {permissibleValues(activeType).length === 0 ? (
                // Free-text type (e.g. user.custom_label): no enumerated values,
                // so let the user type the label and save it directly.
                <form
                  className="flex flex-col gap-2 px-3 py-2"
                  onSubmit={(e) => {
                    e.preventDefault();
                    const v = freeText.trim();
                    if (!v || saving) return;
                    void confirm(activeType, v);
                  }}
                >
                  <label
                    htmlFor={freeTextId}
                    className="text-[length:var(--fs-body)] text-ink-3"
                  >
                    Type a label, then press Enter or Save.
                  </label>
                  <input
                    id={freeTextId}
                    type="text"
                    autoFocus
                    value={freeText}
                    disabled={saving}
                    onChange={(e) => setFreeText(e.target.value)}
                    placeholder="e.g. good handoff"
                    className="w-full border border-rule bg-surface px-2 py-1 text-[length:var(--fs-body)] text-ink placeholder:text-ink-4 focus-mono disabled:opacity-50"
                  />
                  <button
                    type="submit"
                    disabled={saving || freeText.trim() === ''}
                    className="inline-flex items-center justify-center gap-1.5 self-end border border-rule px-2.5 py-1 text-[length:var(--fs-label)] text-ink hover:bg-surface-hover focus-mono transition-colors disabled:opacity-50 disabled:hover:bg-transparent"
                  >
                    {saving && <Loader2 aria-hidden="true" className="size-[var(--ic-sm)] animate-spin motion-reduce:animate-none text-ink-3" />}
                    {saving ? 'saving' : 'save'}
                  </button>
                </form>
              ) : (
                permissibleValues(activeType).map((value) => (
                  <button
                    key={value}
                    type="button"
                    disabled={saving}
                    onClick={() => confirm(activeType, value)}
                    className="flex items-center justify-between px-3 py-2 text-left text-[length:var(--fs-body)] text-ink hover:bg-surface-hover focus-mono transition-colors disabled:opacity-50"
                  >
                    <span className="font-mono">{value}</span>
                    {saving && <Loader2 aria-hidden="true" className="size-[var(--ic-sm)] animate-spin motion-reduce:animate-none text-ink-3" />}
                    {!saving && <Check aria-hidden="true" className="size-[var(--ic-sm)] text-ink-4 opacity-0" />}
                  </button>
                ))
              )}
              {error && (
                <p className="px-3 py-2 text-[length:var(--fs-body)] text-danger border-t border-rule">
                  {error}
                </p>
              )}
            </div>
          )}
        </div>
      )}
    </span>
  );
}
