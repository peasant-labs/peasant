'use client';

import { useCallback, useEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react';
import { Check, Crosshair, MessageSquarePlus, X } from 'lucide-react';
import {
  feedbackDisabled,
  feedbackLabelFor,
  feedbackSectionOf,
  feedbackSelectorFor,
  feedbackTargetFrom,
  isIgnoredFeedbackElement,
  isTooLargeRect,
} from '@/lib/feedback/domTarget';
import type { LegacyDomTarget } from '@/lib/feedback/types';

/**
 * In-page inspect + user feedback tool — a development iteration aid.
 *
 * A note is appended to `llm/ui-feedback.md` through the Peasant Local API route
 * `POST /api/v1/local/feedback`, which owns the markdown shape. The flow is one
 * sweep with no panels:
 *
 *   press "comment" (or `c`)   -> the DOM picker arms: the hovered element gets an
 *                                 outline and a cheap tag near the pointer
 *   click any element          -> the note popup opens on the right, focused
 *   write, then cmd/ctrl+Enter -> the note is appended and the picker re-arms
 *   esc                        -> backs out one step (cancel the note, then disarm)
 *
 * `?fb=off` disables the tool for the page (the capture scripts run that way).
 *
 * The hover path never reads `textContent` and never measures layout: the
 * highlight is an outline on the element itself and the tag is placed from the
 * pointer coordinates, so sweeping the pointer stays free of synchronous reflow.
 * The full element description (selector + owning section + DOM snippet) is built
 * once, on pick.
 */

/** Peasant Local API route that appends a UI annotation to `llm/ui-feedback.md`. */
const FEEDBACK_ROUTE = '/api/v1/local/feedback';
const FEEDBACK_SAVED_TOAST = 'saved to llm/ui-feedback.md';
/** The popup may not sit under the persistent product header. */
const POPUP_MIN_TOP = 64;
const POPUP_BOTTOM_RESERVE = 260;
const TOAST_MS = 2500;
const SAVE_SHORTCUT = '⌘↵';

/** The element description posted with a note. */
export type FeedbackTarget = LegacyDomTarget;

/** The request body of `POST /api/v1/local/feedback`. */
export interface FeedbackPayload {
  route: string;
  anchor: string;
  selector: string;
  snippet: string;
  comment: string;
}

export {
  feedbackDisabled,
  feedbackLabelFor,
  feedbackSectionOf,
  feedbackSelectorFor,
  feedbackTargetFrom,
};

/** Builds the request body for a picked element. */
export function feedbackPayloadFor(el: Element, route: string, comment: string): FeedbackPayload {
  const target = feedbackTargetFrom(el);
  return {
    route,
    // the owning section leads; the cheap label is the fallback when nothing scopes it
    anchor: target.section || target.label,
    selector: target.selector,
    snippet: target.snippet,
    comment: comment.trim(),
  };
}

interface ComposeState {
  /** `section  selector`, the target line shown in the popup. */
  desc: string;
  /** Viewport top of the popup, near the picked element. */
  top: number;
}

function InspectFeedbackTool() {
  const [armed, setArmed] = useState(false);
  const [compose, setCompose] = useState<ComposeState | null>(null);
  const [toast, setToast] = useState<string | null>(null);
  const tagRef = useRef<HTMLDivElement>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  /** The picked element, kept out of state so a pick never re-renders the page. */
  const targetRef = useRef<Element | null>(null);

  /** Re-arm the picker and drop the open note. Used by save, cancel, and esc. */
  const endCompose = useCallback(() => {
    setCompose(null);
    targetRef.current = null;
    setArmed(true);
  }, []);

  const saveComment = useCallback(() => {
    const note = (textareaRef.current?.value ?? '').trim();
    if (!note) {
      textareaRef.current?.focus();
      return;
    }
    const el = targetRef.current;
    if (!el) {
      endCompose();
      return;
    }
    const payload = feedbackPayloadFor(el, window.location.pathname, note);
    fetch(FEEDBACK_ROUTE, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    })
      .then((res) => {
        if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
        setToast(FEEDBACK_SAVED_TOAST);
        // Re-arm so the next element is one click away. The note is dropped only on
        // success: the popup unmounts, and a failed save keeps the text for a retry.
        endCompose();
      })
      .catch((err: unknown) => {
        setToast(`error: ${err instanceof Error ? err.message : 'save failed'}`);
      });
  }, [endCompose]);

  // Shortcuts: `c` toggles the picker (never while typing), esc backs out one step,
  // cmd/ctrl+Enter (bound on the textarea) saves. Rebinds on armed/compose so the
  // handlers always read current state.
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        if (compose) {
          event.preventDefault();
          endCompose();
        } else if (armed) {
          setArmed(false);
        }
        return;
      }
      if ((event.key === 'c' || event.key === 'C') && !event.metaKey && !event.ctrlKey && !event.altKey && !compose) {
        const active = document.activeElement as HTMLElement | null;
        if (active && (active.tagName === 'INPUT' || active.tagName === 'TEXTAREA' || active.isContentEditable)) return;
        event.preventDefault();
        setArmed((current) => !current);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [armed, compose, endCompose]);

  // Picker: hover highlights an element, click selects it. Listeners exist only while
  // armed and are captured, so a click selects instead of activating the underlying
  // control. The tool's own chrome carries `data-fb` and is skipped, as are page-sized
  // wrappers (measured once, on click — the hover path stays layout-free).
  useEffect(() => {
    if (!armed) return;
    document.body.classList.add('fbk-arming');

    let rafId = 0;
    let pending: Element | null = null;
    let hoverEl: Element | null = null;
    let outlined: Element | null = null;
    let pointerX = 0;
    let pointerY = 0;

    const clearHover = () => {
      if (outlined) {
        outlined.classList.remove('fbk-target');
        outlined = null;
      }
      hoverEl = null;
      pending = null;
      if (tagRef.current) tagRef.current.style.display = 'none';
    };

    const paint = () => {
      rafId = 0;
      const el = pending;
      pending = null;
      if (!el) return;
      const tag = tagRef.current;
      if (tag) {
        // Label BEFORE adding our own class, or the tag would show `fbk-target`.
        const section = feedbackSectionOf(el);
        tag.textContent = (section ? section + ' / ' : '') + feedbackLabelFor(el);
        tag.style.left = `${Math.max(2, Math.min(pointerX + 12, window.innerWidth - 220))}px`;
        tag.style.top = `${Math.max(2, pointerY - 24)}px`;
        tag.style.display = 'block';
      }
      if (outlined && outlined !== el) outlined.classList.remove('fbk-target');
      el.classList.add('fbk-target');
      outlined = el;
    };

    const onMove = (event: MouseEvent) => {
      pointerX = event.clientX;
      pointerY = event.clientY;
      const el = event.target instanceof Element ? event.target : null;
      if (!el) {
        clearHover();
        return;
      }
      if (el === hoverEl) return;
      if (isIgnoredFeedbackElement(el)) {
        clearHover();
        return;
      }
      hoverEl = el;
      pending = el;
      if (!rafId) rafId = requestAnimationFrame(paint);
    };

    const onClick = (event: MouseEvent) => {
      const el = event.target instanceof Element ? event.target : null;
      if (
        !el ||
        isIgnoredFeedbackElement(el) ||
        isTooLargeRect(el.getBoundingClientRect(), { width: window.innerWidth, height: window.innerHeight })
      ) {
        return;
      }
      event.preventDefault();
      event.stopPropagation();
      // Drop the highlight before describing the element, or the description would
      // capture our own `fbk-target` class.
      if (outlined) {
        outlined.classList.remove('fbk-target');
        outlined = null;
      }
      el.classList.remove('fbk-target');
      targetRef.current = el;
      const target = feedbackTargetFrom(el);
      const rect = el.getBoundingClientRect();
      const top = Math.min(
        Math.max(rect.top, POPUP_MIN_TOP),
        Math.max(POPUP_MIN_TOP, window.innerHeight - POPUP_BOTTOM_RESERVE),
      );
      setArmed(false); // freeze hovering while the note is open
      setCompose({ desc: (target.section ? target.section + '  ' : '') + target.selector, top });
      window.setTimeout(() => textareaRef.current?.focus(), 30);
    };

    document.addEventListener('mousemove', onMove, true);
    document.addEventListener('click', onClick, true);
    window.addEventListener('scroll', clearHover, true);
    return () => {
      document.body.classList.remove('fbk-arming');
      if (rafId) cancelAnimationFrame(rafId);
      clearHover();
      document.removeEventListener('mousemove', onMove, true);
      document.removeEventListener('click', onClick, true);
      window.removeEventListener('scroll', clearHover, true);
    };
  }, [armed]);

  useEffect(() => {
    if (!toast) return;
    const timer = window.setTimeout(() => setToast(null), TOAST_MS);
    return () => window.clearTimeout(timer);
  }, [toast]);

  const onComposeKey = (event: ReactKeyboardEvent<HTMLTextAreaElement>) => {
    if ((event.metaKey || event.ctrlKey) && event.key === 'Enter') {
      event.preventDefault();
      saveComment();
    }
  };

  return (
    <>
      <button
        type="button"
        className={'fbk-launch' + (armed ? ' armed' : '')}
        data-fb=""
        aria-pressed={armed}
        aria-label={armed ? 'stop selecting an element to comment on (esc)' : 'comment on an element (c)'}
        onClick={() => {
          if (armed) {
            setArmed(false);
          } else {
            setCompose(null);
            setArmed(true);
          }
        }}
      >
        {armed ? (
          <>
            <Crosshair aria-hidden="true" /> selecting <span className="cnt">esc</span>
          </>
        ) : (
          <>
            <MessageSquarePlus aria-hidden="true" /> comment <span className="cnt">c</span>
          </>
        )}
      </button>

      <div ref={tagRef} className="fbk-tag" data-fb="" />

      {compose && (
        <div className="fbk-pop" data-fb="" style={{ top: compose.top }}>
          <div className="fbk-pop-h" data-fb="">
            <span>
              <Crosshair aria-hidden="true" /> new comment
            </span>
            <button type="button" className="fbk-x" data-fb="" aria-label="cancel (esc)" onClick={endCompose}>
              <X aria-hidden="true" />
            </button>
          </div>
          <div className="fbk-pop-b" data-fb="">
            <div className="fbk-pop-tgt" data-fb="">
              {compose.desc}
            </div>
            <textarea
              ref={textareaRef}
              className="input"
              data-fb=""
              rows={4}
              placeholder="what should change here?"
              onKeyDown={onComposeKey}
            />
          </div>
          <div className="fbk-pop-f" data-fb="">
            <button type="button" className="btn btn-ghost btn-sm" data-fb="" onClick={endCompose}>
              cancel
            </button>
            <button type="button" className="btn btn-primary btn-sm" data-fb="" onClick={saveComment}>
              <Check aria-hidden="true" /> save <span className="fbk-kbd">{SAVE_SHORTCUT}</span>
            </button>
          </div>
        </div>
      )}

      {toast && (
        <div
          role="status"
          data-fb=""
          className="fixed right-4 bottom-20 z-[65] border border-rule-strong bg-surface px-3.5 py-2 font-mono text-[11px] text-ink shadow-[var(--glow-soft)]"
        >
          {toast}
        </div>
      )}
    </>
  );
}

/**
 * Mounts the dev feedback tool, unless the page asked for it to stay off with
 * `?fb=off`. The switch is read after mount so the server render and the first
 * client render agree.
 */
export function DevAnnotateOverlay() {
  const [enabled, setEnabled] = useState(false);

  useEffect(() => {
    const sync = () => setEnabled(!feedbackDisabled(window.location.search));
    sync();
    window.addEventListener('popstate', sync);
    return () => window.removeEventListener('popstate', sync);
  }, []);

  if (!enabled) return null;
  return <InspectFeedbackTool />;
}
