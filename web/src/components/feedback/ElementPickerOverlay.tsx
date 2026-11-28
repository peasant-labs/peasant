'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import {
  PickerDirection,
  type ElementBox,
  type ElementPickerAdapter,
  type PickedElementDraft,
  type ViewportSize,
} from '@/lib/feedback/types';
import './feedback-widget.css';

export interface ElementPickerOverlayProps {
  active: boolean;
  adapter: ElementPickerAdapter;
  onConfirm: (el: Element, info: PickedElementDraft) => void;
  onCancel: () => void;
}

export const ElementPickerLabels = Object.freeze({
  StatusHint: 'pick an element · enter to choose · esc to cancel',
} as const);

function directionForKey(event: KeyboardEvent): PickerDirection | null {
  switch (event.key) {
    case 'ArrowUp':
      return PickerDirection.Parent;
    case 'ArrowDown':
      return PickerDirection.Child;
    case 'ArrowLeft':
      return PickerDirection.PrevSibling;
    case 'ArrowRight':
      return PickerDirection.NextSibling;
    case 'Tab':
      return event.shiftKey ? PickerDirection.PrevLandmark : PickerDirection.NextLandmark;
    default:
      return null;
  }
}

function toBox(rect: ElementBox): ElementBox {
  return {
    top: rect.top,
    left: rect.left,
    width: rect.width,
    height: rect.height,
  };
}

function sameBox(a: ElementBox | null, b: ElementBox): boolean {
  return Boolean(a && a.top === b.top && a.left === b.left && a.width === b.width && a.height === b.height);
}

/**
 * Full-viewport element picker overlay using four non-interactive scrim rects
 * around the highlighted target, an amber focus ring, and keyboard traversal.
 */
export function ElementPickerOverlay({ active, adapter, onConfirm, onCancel }: ElementPickerOverlayProps) {
  const [mounted, setMounted] = useState(false);
  const [box, setBox] = useState<ElementBox | null>(null);
  const [viewport, setViewport] = useState<ViewportSize>({ width: 0, height: 0 });
  const [announcement, setAnnouncement] = useState('');

  const targetElRef = useRef<Element | null>(null);
  const adapterRef = useRef(adapter);
  adapterRef.current = adapter;
  const onConfirmRef = useRef(onConfirm);
  onConfirmRef.current = onConfirm;
  const onCancelRef = useRef(onCancel);
  onCancelRef.current = onCancel;

  useEffect(() => setMounted(true), []);

  const applyTarget = useCallback((el: Element | null, measuredRect?: DOMRect) => {
    targetElRef.current = el;
    if (!el) {
      setBox(null);
      setAnnouncement('');
      return;
    }
    const r = measuredRect ?? el.getBoundingClientRect();
    const nextBox = toBox(r);
    setBox((prev) => (sameBox(prev, nextBox) ? prev : nextBox));
    const currentAdapter = adapterRef.current;
    const label = currentAdapter.labelFor(el);
    const section = currentAdapter.sectionOf(el);
    const dims = `${Math.round(r.width)} by ${Math.round(r.height)}`;
    setAnnouncement(section ? `${label} in ${section}, ${dims}` : `${label}, ${dims}`);
  }, []);

  useEffect(() => {
    if (!active) {
      applyTarget(null);
      return;
    }

    setViewport({ width: window.innerWidth, height: window.innerHeight });

    let rafId = 0;

    const onPointerMove = (event: PointerEvent) => {
      const raw = event.target instanceof Element ? event.target : null;
      const currentAdapter = adapterRef.current;
      if (!raw || currentAdapter.isIgnoredElement(raw)) {
        if (targetElRef.current !== null) applyTarget(null);
        return;
      }
      const r = raw.getBoundingClientRect();
      const vp: ViewportSize = { width: window.innerWidth, height: window.innerHeight };
      if (currentAdapter.isTooLargeRect(r, vp)) {
        if (targetElRef.current !== null) applyTarget(null);
        return;
      }
      if (raw !== targetElRef.current) {
        applyTarget(raw, r);
      }
    };

    const onClick = (event: MouseEvent) => {
      const raw = event.target instanceof Element ? event.target : null;
      const currentAdapter = adapterRef.current;
      event.preventDefault();
      event.stopPropagation();
      if (!raw || currentAdapter.isIgnoredElement(raw)) return;
      const r = raw.getBoundingClientRect();
      const vp: ViewportSize = { width: window.innerWidth, height: window.innerHeight };
      if (currentAdapter.isTooLargeRect(r, vp)) return;
      onConfirmRef.current(raw, currentAdapter.describe(raw));
    };

    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        event.stopPropagation();
        onCancelRef.current();
        return;
      }
      if (event.key === 'Enter' || event.key === ' ') {
        event.preventDefault();
        event.stopPropagation();
        const current = targetElRef.current;
        const currentAdapter = adapterRef.current;
        if (current && !currentAdapter.isIgnoredElement(current)) {
          onConfirmRef.current(current, currentAdapter.describe(current));
        }
        return;
      }
      const direction = directionForKey(event);
      if (direction === null) return;
      event.preventDefault();
      event.stopPropagation();
      const currentAdapter = adapterRef.current;
      const next = currentAdapter.step(targetElRef.current, direction, document);
      if (!next || currentAdapter.isIgnoredElement(next)) return;
      if (typeof next.scrollIntoView === 'function') {
        next.scrollIntoView({ block: 'nearest', inline: 'nearest' });
      }
      applyTarget(next);
    };

    const scheduleMeasure = () => {
      if (rafId) return;
      rafId = window.requestAnimationFrame(() => {
        rafId = 0;
        setViewport({ width: window.innerWidth, height: window.innerHeight });
        const current = targetElRef.current;
        if (!current || !current.isConnected) {
          applyTarget(null);
          return;
        }
        const r = current.getBoundingClientRect();
        const nextBox = toBox(r);
        setBox((prev) => (sameBox(prev, nextBox) ? prev : nextBox));
      });
    };

    document.addEventListener('pointermove', onPointerMove, true);
    document.addEventListener('click', onClick, true);
    document.addEventListener('keydown', onKeyDown, true);
    window.addEventListener('scroll', scheduleMeasure, true);
    window.addEventListener('resize', scheduleMeasure);

    return () => {
      if (rafId) window.cancelAnimationFrame(rafId);
      document.removeEventListener('pointermove', onPointerMove, true);
      document.removeEventListener('click', onClick, true);
      document.removeEventListener('keydown', onKeyDown, true);
      window.removeEventListener('scroll', scheduleMeasure, true);
      window.removeEventListener('resize', scheduleMeasure);
    };
  }, [active, applyTarget]);

  if (!mounted || !active) return null;

  const vw = viewport.width || window.innerWidth;
  const vh = viewport.height || window.innerHeight;

  return createPortal(
    <div className="fbw-picker-root" data-feedback-widget="">
      {box ? (
        <>
          <span
            className="fbw-picker-scrim"
            aria-hidden="true"
            style={{ top: 0, left: 0, width: vw, height: Math.max(0, box.top) }}
          />
          <span
            className="fbw-picker-scrim"
            aria-hidden="true"
            style={{
              top: box.top + box.height,
              left: 0,
              width: vw,
              height: Math.max(0, vh - (box.top + box.height)),
            }}
          />
          <span
            className="fbw-picker-scrim"
            aria-hidden="true"
            style={{
              top: box.top,
              left: 0,
              width: Math.max(0, box.left),
              height: Math.max(0, box.height),
            }}
          />
          <span
            className="fbw-picker-scrim"
            aria-hidden="true"
            style={{
              top: box.top,
              left: box.left + box.width,
              width: Math.max(0, vw - (box.left + box.width)),
              height: Math.max(0, box.height),
            }}
          />
          <span
            className="fbw-picker-ring"
            aria-hidden="true"
            style={{
              top: box.top,
              left: box.left,
              width: Math.max(0, box.width),
              height: Math.max(0, box.height),
            }}
          />
        </>
      ) : (
        <span
          className="fbw-picker-scrim"
          aria-hidden="true"
          style={{ top: 0, left: 0, width: vw, height: vh }}
        />
      )}

      <div className="fbw-picker-status" role="status" aria-hidden="true">
        {ElementPickerLabels.StatusHint}
      </div>

      <div className="sr-only fbw-sr-only" role="status" aria-live="polite">
        {announcement}
      </div>
    </div>,
    document.body,
  );
}
