'use client';

import { useCallback, useEffect, useId, useRef, useState, type KeyboardEvent as ReactKeyboardEvent, type Ref } from 'react';
import { Bug, Lightbulb, MessageSquarePlus } from 'lucide-react';
import { ALL_FEEDBACK_KINDS, FeedbackKind } from '@/lib/feedback/types';
import './feedback-widget.css';

export interface FeedbackLauncherProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onSelectKind: (kind: FeedbackKind) => void;
  disabled?: boolean;
  launcherRef?: Ref<HTMLButtonElement>;
}

function assignRef(ref: Ref<HTMLButtonElement> | undefined, node: HTMLButtonElement | null): void {
  if (!ref) return;
  if (typeof ref === 'function') {
    ref(node);
    return;
  }
  (ref as { current: HTMLButtonElement | null }).current = node;
}

function KindIcon({ kind }: { kind: FeedbackKind }) {
  switch (kind.wire) {
    case FeedbackKind.Problem.wire:
      return <Bug size={16} aria-hidden="true" />;
    case FeedbackKind.Feature.wire:
      return <Lightbulb size={16} aria-hidden="true" />;
    default: {
      const exhaustive: never = kind;
      return exhaustive;
    }
  }
}

const TILE_POSITION_CLASS: Readonly<Record<FeedbackKind['wire'], string>> = {
  [FeedbackKind.Problem.wire]: 'fbw-menu-item--above',
  [FeedbackKind.Feature.wire]: 'fbw-menu-item--left',
};

/**
 * Fixed bottom-right feedback launcher with a 2-tile radial WAI-ARIA menu.
 */
export function FeedbackLauncher({ open, onOpenChange, onSelectKind, disabled = false, launcherRef }: FeedbackLauncherProps) {
  const menuId = useId();
  const rootRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const itemRefs = useRef<(HTMLButtonElement | null)[]>([]);
  const pendingFocusIndex = useRef<number | null>(null);
  const [activeIndex, setActiveIndex] = useState(0);

  const setTriggerRef = useCallback(
    (node: HTMLButtonElement | null) => {
      triggerRef.current = node;
      assignRef(launcherRef, node);
    },
    [launcherRef],
  );

  const focusItem = useCallback((index: number) => {
    const count = ALL_FEEDBACK_KINDS.length;
    const nextIndex = ((index % count) + count) % count;
    setActiveIndex(nextIndex);
    itemRefs.current[nextIndex]?.focus();
  }, []);

  useEffect(() => {
    if (!open) return;
    const targetIndex = pendingFocusIndex.current ?? 0;
    pendingFocusIndex.current = null;
    setActiveIndex(targetIndex);
    const frame = window.requestAnimationFrame(() => {
      itemRefs.current[targetIndex]?.focus();
    });
    return () => window.cancelAnimationFrame(frame);
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onPointerDown = (event: PointerEvent) => {
      const target = event.target;
      if (target instanceof Node && !rootRef.current?.contains(target)) {
        onOpenChange(false);
      }
    };
    document.addEventListener('pointerdown', onPointerDown);
    return () => document.removeEventListener('pointerdown', onPointerDown);
  }, [open, onOpenChange]);

  const onTriggerKeyDown = (event: ReactKeyboardEvent<HTMLButtonElement>) => {
    if (disabled) return;
    if (event.key === 'ArrowDown') {
      event.preventDefault();
      if (open) {
        focusItem(0);
      } else {
        pendingFocusIndex.current = 0;
        onOpenChange(true);
      }
      return;
    }
    if (event.key === 'ArrowUp') {
      event.preventDefault();
      const last = ALL_FEEDBACK_KINDS.length - 1;
      if (open) {
        focusItem(last);
      } else {
        pendingFocusIndex.current = last;
        onOpenChange(true);
      }
      return;
    }
    if (event.key === 'Escape' && open) {
      event.preventDefault();
      onOpenChange(false);
      triggerRef.current?.focus();
    }
  };

  const onMenuKeyDown = (event: ReactKeyboardEvent<HTMLDivElement>) => {
    const count = ALL_FEEDBACK_KINDS.length;
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      onOpenChange(false);
      triggerRef.current?.focus();
      return;
    }
    if (event.key === 'Tab') {
      onOpenChange(false);
      return;
    }
    if (event.key === 'ArrowDown' || event.key === 'ArrowRight') {
      event.preventDefault();
      focusItem((activeIndex + 1) % count);
      return;
    }
    if (event.key === 'ArrowUp' || event.key === 'ArrowLeft') {
      event.preventDefault();
      focusItem((activeIndex - 1 + count) % count);
      return;
    }
    if (event.key === 'Home') {
      event.preventDefault();
      focusItem(0);
      return;
    }
    if (event.key === 'End') {
      event.preventDefault();
      focusItem(count - 1);
    }
  };

  const onItemKeyDown = (kind: FeedbackKind) => (event: ReactKeyboardEvent<HTMLButtonElement>) => {
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      onSelectKind(kind);
    }
  };

  return (
    <div ref={rootRef} className="fbw-launcher-root" data-feedback-widget="">
      <button
        ref={setTriggerRef}
        type="button"
        className="fbw-launcher"
        aria-label="feedback"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={menuId}
        disabled={disabled}
        onClick={() => onOpenChange(!open)}
        onKeyDown={onTriggerKeyDown}
      >
        <MessageSquarePlus size={16} aria-hidden="true" />
      </button>

      <div
        id={menuId}
        role="menu"
        aria-label="feedback options"
        className="fbw-menu"
        hidden={!open}
        onKeyDown={onMenuKeyDown}
      >
        {ALL_FEEDBACK_KINDS.map((kind, index) => (
          <button
            key={kind.wire}
            ref={(node) => {
              itemRefs.current[index] = node;
            }}
            type="button"
            role="menuitem"
            tabIndex={index === activeIndex ? 0 : -1}
            className={`fbw-menu-item ${TILE_POSITION_CLASS[kind.wire]}`}
            onClick={() => onSelectKind(kind)}
            onKeyDown={onItemKeyDown(kind)}
          >
            <KindIcon kind={kind} />
            <span>{kind.label}</span>
          </button>
        ))}
      </div>
    </div>
  );
}
