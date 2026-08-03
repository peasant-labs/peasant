'use client';

import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from 'react';
import { createPortal } from 'react-dom';
import { cn } from '@/lib/utils';
import type { TourStep } from './steps';

/** Padding (px) between the highlighted element and the spotlight ring. */
const SPOTLIGHT_PADDING = 8;
/** Gap (px) between the spotlight and the popover. */
const POPOVER_GAP = 12;
/** Approximate popover width used for horizontal clamping. */
const POPOVER_WIDTH = 320;

interface Rect {
  top: number;
  left: number;
  width: number;
  height: number;
}

interface TourOverlayProps {
  step: TourStep;
  /** Bounding rect of the highlighted target, or `null` to dim full screen. */
  targetRect: Rect | null;
  stepIndex: number;
  stepCount: number;
  isFirst: boolean;
  isLast: boolean;
  onNext: () => void;
  onPrev: () => void;
  onSkip: () => void;
  /** Toggles the "don't show again" intent (purely informational copy here —
   *  skip/complete already persist; this lets the user opt out explicitly). */
}

/** Clamp a value into [min, max]. */
function clamp(v: number, min: number, max: number): number {
  return Math.min(max, Math.max(min, v));
}

export function TourOverlay({
  step,
  targetRect,
  stepIndex,
  stepCount,
  isFirst,
  isLast,
  onNext,
  onPrev,
  onSkip,
}: TourOverlayProps) {
  const popoverRef = useRef<HTMLDivElement>(null);
  const nextBtnRef = useRef<HTMLButtonElement>(null);
  const [mounted, setMounted] = useState(false);

  useEffect(() => setMounted(true), []);

  // Move focus into the popover when the step changes; restore on unmount.
  useLayoutEffect(() => {
    const prevFocus = document.activeElement as HTMLElement | null;
    nextBtnRef.current?.focus();
    return () => {
      prevFocus?.focus?.();
    };
    // Re-run when the step index changes so each step re-focuses its primary
    // action.
  }, [stepIndex]);

  // Keyboard: Esc skips, ArrowRight/Enter advances, ArrowLeft goes back, and
  // Tab is trapped within the popover.
  const onKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault();
        onSkip();
        return;
      }
      if (e.key === 'ArrowRight') {
        e.preventDefault();
        onNext();
        return;
      }
      if (e.key === 'ArrowLeft') {
        e.preventDefault();
        if (!isFirst) onPrev();
        return;
      }
      if (e.key === 'Tab') {
        const root = popoverRef.current;
        if (!root) return;
        const focusables = root.querySelectorAll<HTMLElement>(
          'button:not([disabled]), [href], [tabindex]:not([tabindex="-1"])',
        );
        if (focusables.length === 0) return;
        const first = focusables[0];
        const last = focusables[focusables.length - 1];
        const activeEl = document.activeElement;
        if (e.shiftKey && activeEl === first) {
          e.preventDefault();
          last.focus();
        } else if (!e.shiftKey && activeEl === last) {
          e.preventDefault();
          first.focus();
        }
      }
    },
    [isFirst, onNext, onPrev, onSkip],
  );

  if (!mounted) return null;

  const vw = window.innerWidth;
  const vh = window.innerHeight;

  // Spotlight geometry. With no target we dim the whole viewport and center the
  // popover.
  const ring = targetRect
    ? {
        top: targetRect.top - SPOTLIGHT_PADDING,
        left: targetRect.left - SPOTLIGHT_PADDING,
        width: targetRect.width + SPOTLIGHT_PADDING * 2,
        height: targetRect.height + SPOTLIGHT_PADDING * 2,
      }
    : null;

  // Popover placement: below the ring if there's room, otherwise above; fall
  // back to centered when there's no target.
  let popoverStyle: React.CSSProperties;
  if (ring) {
    const spaceBelow = vh - (ring.top + ring.height);
    const placeBelow = spaceBelow > 200 || spaceBelow > ring.top;
    const top = placeBelow
      ? ring.top + ring.height + POPOVER_GAP
      : undefined;
    const bottom = placeBelow ? undefined : vh - ring.top + POPOVER_GAP;
    const left = clamp(
      ring.left + ring.width / 2 - POPOVER_WIDTH / 2,
      12,
      Math.max(12, vw - POPOVER_WIDTH - 12),
    );
    popoverStyle = { position: 'fixed', top, bottom, left, width: POPOVER_WIDTH };
  } else {
    popoverStyle = {
      position: 'fixed',
      top: '50%',
      left: '50%',
      transform: 'translate(-50%, -50%)',
      width: POPOVER_WIDTH,
    };
  }

  return createPortal(
    <div
      className="fixed inset-0 z-[100] animate-fade-in"
      role="dialog"
      aria-modal="true"
      aria-label={`Product tour: ${step.title}`}
      onKeyDown={onKeyDown}
    >
      {/* Dark backdrop with a transparent cut-out over the target. We render
          four panels around the ring so the highlighted element stays fully
          interactive-looking (and visually un-dimmed) without an SVG mask. */}
      {ring ? (
        <>
          {/* top */}
          <Backdrop top={0} left={0} width={vw} height={Math.max(0, ring.top)} />
          {/* bottom */}
          <Backdrop
            top={ring.top + ring.height}
            left={0}
            width={vw}
            height={Math.max(0, vh - (ring.top + ring.height))}
          />
          {/* left */}
          <Backdrop
            top={ring.top}
            left={0}
            width={Math.max(0, ring.left)}
            height={ring.height}
          />
          {/* right */}
          <Backdrop
            top={ring.top}
            left={ring.left + ring.width}
            width={Math.max(0, vw - (ring.left + ring.width))}
            height={ring.height}
          />
          {/* highlight ring */}
          <div
            aria-hidden
            className="pointer-events-none fixed border-2 border-mark"
            style={{
              top: ring.top,
              left: ring.left,
              width: ring.width,
              height: ring.height,
            }}
          />
        </>
      ) : (
        <Backdrop top={0} left={0} width={vw} height={vh} />
      )}

      {/* Popover */}
      <div
        ref={popoverRef}
        style={popoverStyle}
        className="border border-rule bg-surface-elev text-ink"
      >
        <div className="px-5 pt-4 pb-3 flex flex-col gap-2">
          <div className="flex items-center justify-between">
            <p className="v2-eyebrow">
              Step {stepIndex + 1} of {stepCount}
            </p>
            <button
              type="button"
              onClick={onSkip}
              className="text-xs text-ink-3 hover:text-ink transition-colors focus-mono cursor-pointer"
            >
              Skip tour
            </button>
          </div>
          <h2 className="font-[family-name:var(--font-display)] text-base font-semibold text-ink">
            {step.title}
          </h2>
          <p className="text-[13px] leading-relaxed text-ink-2">{step.body}</p>
        </div>

        <div className="flex items-center justify-between border-t border-rule px-5 py-3">
          {/* Progress dots */}
          <div className="flex items-center gap-1.5" aria-hidden>
            {Array.from({ length: stepCount }).map((_, i) => (
              <span
                key={i}
                className={cn(
                  'h-1.5 w-1.5',
                  i === stepIndex ? 'bg-mark' : 'bg-rule-strong',
                )}
              />
            ))}
          </div>

          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={onPrev}
              disabled={isFirst}
              className="h-8 px-3 text-sm font-medium border border-rule bg-surface text-ink hover:bg-surface-hover disabled:opacity-40 disabled:pointer-events-none focus-mono cursor-pointer transition-colors"
            >
              Back
            </button>
            <button
              ref={nextBtnRef}
              type="button"
              onClick={onNext}
              className="h-8 px-3 text-sm font-medium bg-mark text-mark-fg hover:bg-mark/90 focus-mono cursor-pointer transition-colors"
            >
              {isLast ? 'Finish' : 'Next'}
            </button>
          </div>
        </div>

        <div className="border-t border-rule px-5 py-2.5">
          <button
            type="button"
            onClick={onSkip}
            className="text-xs text-ink-3 hover:text-ink transition-colors focus-mono cursor-pointer"
          >
            Don’t show this again
          </button>
        </div>
      </div>
    </div>,
    document.body,
  );
}

/** A single dimming panel. `pointer-events` are on so clicks don't fall through
 *  to the page behind the overlay. */
function Backdrop({ top, left, width, height }: Rect) {
  return (
    <div
      aria-hidden
      className="fixed bg-black/55"
      style={{ top, left, width, height }}
    />
  );
}
