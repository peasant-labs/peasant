'use client';

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from 'react';
import { usePathname, useRouter } from 'next/navigation';
import { useTourState, type TourControls } from '@/hooks/useTourState';
import { TourOverlay } from './TourOverlay';
import { TOUR_STEPS, TOUR_STEP_COUNT, type TourStep } from './steps';

interface TourContextValue {
  /** Start the tour manually (e.g. from a "Take the tour" affordance). */
  start: () => void;
  /** True once the tour has been completed or skipped. */
  completed: boolean;
  /** True while the tour overlay is showing. */
  active: boolean;
}

const TourContext = createContext<TourContextValue | null>(null);

/** Access the tour controls. Must be called within a {@link TourProvider}. */
export function useTour(): TourContextValue {
  const ctx = useContext(TourContext);
  if (!ctx) {
    throw new Error('useTour must be used within a TourProvider');
  }
  return ctx;
}

interface Rect {
  top: number;
  left: number;
  width: number;
  height: number;
}

/** Max time (ms) to wait for a step's anchor to mount before skipping it. */
const RESOLVE_TIMEOUT_MS = 2500;

function selectorFor(step: TourStep): string {
  return `[data-tour="${step.anchor}"]`;
}

/**
 * Compare two pathnames ignoring a trailing slash. The app sets
 * `trailingSlash: true`, so `usePathname()` returns `/projects/` while our
 * step paths are written `/projects`; without normalising, the equality check
 * would always miss and we'd re-`push` on every frame.
 */
function samePath(a: string, b: string): boolean {
  const norm = (p: string) => (p.length > 1 ? p.replace(/\/+$/, '') : p);
  return norm(a) === norm(b);
}

function rectOf(el: Element): Rect {
  const r = el.getBoundingClientRect();
  return { top: r.top, left: r.left, width: r.width, height: r.height };
}

export function TourProvider({ children }: { children: ReactNode }) {
  const router = useRouter();
  const pathname = usePathname();
  const tour: TourControls = useTourState({ stepCount: TOUR_STEP_COUNT });

  const [targetRect, setTargetRect] = useState<Rect | null>(null);

  // First-run auto-start is DISABLED for now (user decision, 2026-06-10:
  // "remove the tutorial for now, it's distracting, keep the files"). The
  // tour machinery, steps, and data-tour anchors all stay; flip this back to
  // true to restore the auto-start behavior.
  const TOUR_AUTO_START = false;

  // Auto-start once, on the first ever visit to `/`. `completed` is hydrated
  // from localStorage inside useTourState; we wait for that read (it flips on
  // mount) and only fire on the home route.
  const autoStartHandled = useRef(false);
  useEffect(() => {
    if (!TOUR_AUTO_START) return;
    if (autoStartHandled.current) return;
    if (tour.completed) {
      autoStartHandled.current = true;
      return;
    }
    if (pathname === '/' && !tour.active) {
      autoStartHandled.current = true;
      // Defer one tick so the page has painted before the spotlight measures.
      const id = window.setTimeout(() => tour.start(), 400);
      return () => window.clearTimeout(id);
    }
  }, [pathname, tour, TOUR_AUTO_START]);

  // Resolve + track the current step's anchor whenever the step or route
  // changes. Strategy:
  //   1. Ensure we're on the step's route (static path, or a dynamically
  //      resolved one). Navigating triggers a pathname change → effect re-runs.
  //   2. Poll for the anchor element via rAF until it mounts or we time out.
  //   3. Keep the rect fresh on scroll/resize while the step is shown.
  //   4. If the anchor never appears (e.g. no projects exist), advance.
  const step = tour.active ? TOUR_STEPS[tour.index] : null;

  useEffect(() => {
    if (!step) {
      setTargetRect(null);
      return;
    }

    let raf = 0;
    let timedOut = false;
    const deadline = Date.now() + RESOLVE_TIMEOUT_MS;

    // --- 1. Ensure route -------------------------------------------------
    let desiredPath: string | null = null;
    if (step.route.kind === 'static') {
      desiredPath = step.route.path;
    } else {
      desiredPath = step.route.resolve();
    }

    // For dynamic steps we may need the *previous* route's content to resolve
    // the link (e.g. the projects list must be mounted to read a project row).
    // If we can't resolve a destination yet, keep polling on the current page;
    // the resolver re-runs each frame until the link appears or we time out.
    if (
      desiredPath &&
      step.route.kind === 'static' &&
      !samePath(pathname, desiredPath)
    ) {
      router.push(desiredPath);
      // The pathname change re-runs this effect; bail until then.
      return;
    }

    // --- 2 + 3. Poll for the anchor and track its rect -------------------
    const measure = () => {
      const el = document.querySelector(selectorFor(step));
      if (el) {
        setTargetRect(rectOf(el));
        return true;
      }
      return false;
    };

    const tick = () => {
      if (measure()) {
        // Anchor found — keep tracking via rAF so scroll/resize stays in sync.
        raf = window.requestAnimationFrame(tick);
        return;
      }
      // Not yet on screen. For dynamic steps, try to navigate to the resolved
      // destination once the source link is available.
      if (step.route.kind === 'dynamic') {
        const resolved = step.route.resolve();
        if (resolved && !samePath(pathname, resolved)) {
          router.push(resolved);
          return; // effect re-runs on the pathname change
        }
      }
      if (Date.now() > deadline) {
        timedOut = true;
        // Couldn't find the anchor (e.g. no projects exist) — advance past
        // this step gracefully rather than stalling the tour.
        tour.next();
        return;
      }
      raf = window.requestAnimationFrame(tick);
    };

    raf = window.requestAnimationFrame(tick);

    const onViewportChange = () => {
      if (!timedOut) measure();
    };
    window.addEventListener('scroll', onViewportChange, true);
    window.addEventListener('resize', onViewportChange);

    return () => {
      window.cancelAnimationFrame(raf);
      window.removeEventListener('scroll', onViewportChange, true);
      window.removeEventListener('resize', onViewportChange);
    };
    // `tour` identity is stable enough (memoised controls); we depend on the
    // step index + active flag + pathname which are the real triggers.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step, pathname, router, tour.index, tour.active]);

  // Scroll the anchor into view when a new step activates so the spotlight
  // isn't off-screen.
  useEffect(() => {
    if (!step) return;
    const el = document.querySelector(selectorFor(step));
    if (el) {
      el.scrollIntoView({ behavior: 'smooth', block: 'center' });
    }
  }, [step, tour.index]);

  return (
    <TourContext.Provider
      value={{ start: tour.start, completed: tour.completed, active: tour.active }}
    >
      {children}
      {step && (
        <TourOverlay
          step={step}
          targetRect={targetRect}
          stepIndex={tour.index}
          stepCount={TOUR_STEP_COUNT}
          isFirst={tour.isFirst}
          isLast={tour.isLast}
          onNext={tour.next}
          onPrev={tour.prev}
          onSkip={tour.skip}
        />
      )}
    </TourContext.Provider>
  );
}
