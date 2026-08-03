'use client';

import { useState, useEffect } from 'react';

type Mode = 'idle' | 'selecting' | 'modal';

interface CapturedElement {
  route: string;
  anchor: string;
  selector: string;
  snippet: string;
}

function detectAnchor(el: Element): string {
  if (el.id) return `#${el.id}`;
  for (const attr of ['data-component', 'data-testid']) {
    const v = el.getAttribute(attr);
    if (v) return `[${attr}="${v}"]`;
  }
  const aria = el.getAttribute('aria-label');
  if (aria) return `aria-label="${aria}"`;
  if (el.matches('button, a')) {
    const text = (el.textContent ?? '').trim().slice(0, 60);
    if (text) return `${el.tagName.toLowerCase()}: "${text}"`;
  }
  if (el.matches('h1, h2, h3, h4, h5, h6')) {
    const text = (el.textContent ?? '').trim().slice(0, 60);
    return `${el.tagName.toLowerCase()}: "${text}"`;
  }
  let parent: Element | null = el.parentElement;
  let depth = 0;
  while (parent && depth < 6) {
    const h = parent.querySelector('h1, h2, h3, h4, h5, h6');
    if (h) return `in section: "${(h.textContent ?? '').trim().slice(0, 60)}"`;
    parent = parent.parentElement;
    depth++;
  }
  const cls = typeof el.className === 'string' ? el.className.split(/\s+/)[0] : '';
  return cls ? `${el.tagName.toLowerCase()}.${cls}` : el.tagName.toLowerCase();
}

function buildSelector(el: Element): string {
  const parts: string[] = [];
  let cur: Element | null = el;
  let depth = 0;
  while (cur && depth < 4 && cur.tagName !== 'BODY') {
    const node: Element = cur;
    if (node.id) {
      parts.unshift(`#${node.id}`);
      break;
    }
    const tag = node.tagName.toLowerCase();
    const parentEl: Element | null = node.parentElement;
    if (parentEl) {
      const children: Element[] = Array.from(parentEl.children);
      const sameTag: Element[] = children.filter((c) => c.tagName === node.tagName);
      const idx = sameTag.indexOf(node);
      parts.unshift(sameTag.length > 1 ? `${tag}:nth-of-type(${idx + 1})` : tag);
    } else {
      parts.unshift(tag);
    }
    cur = parentEl;
    depth++;
  }
  return parts.join(' > ');
}

export function DevAnnotateOverlay() {
  const [mode, setMode] = useState<Mode>('idle');
  const [hoveredEl, setHoveredEl] = useState<Element | null>(null);
  const [captured, setCaptured] = useState<CapturedElement | null>(null);
  const [comment, setComment] = useState('');
  const [saving, setSaving] = useState(false);
  const [toast, setToast] = useState<string | null>(null);

  useEffect(() => {
    if (mode !== 'selecting') return;
    document.body.style.cursor = 'crosshair';

    const onMouseOver = (e: MouseEvent) => {
      const target = e.target as Element | null;
      if (!target || target.closest('[data-dev-overlay]')) {
        setHoveredEl(null);
        return;
      }
      setHoveredEl(target);
    };

    const onClick = (e: MouseEvent) => {
      const target = e.target as Element | null;
      if (!target || target.closest('[data-dev-overlay]')) return;
      e.preventDefault();
      e.stopPropagation();
      e.stopImmediatePropagation();
      setCaptured({
        route: window.location.pathname,
        anchor: detectAnchor(target),
        selector: buildSelector(target),
        snippet: target.outerHTML.slice(0, 400),
      });
      setMode('modal');
      setHoveredEl(null);
    };

    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        setMode('idle');
        setHoveredEl(null);
      }
    };

    const onScroll = () => setHoveredEl(null);

    document.addEventListener('mouseover', onMouseOver);
    document.addEventListener('click', onClick, true);
    document.addEventListener('keydown', onKey);
    window.addEventListener('scroll', onScroll, true);

    return () => {
      document.body.style.cursor = '';
      document.removeEventListener('mouseover', onMouseOver);
      document.removeEventListener('click', onClick, true);
      document.removeEventListener('keydown', onKey);
      window.removeEventListener('scroll', onScroll, true);
    };
  }, [mode]);

  useEffect(() => {
    if (mode !== 'modal') return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        setMode('idle');
        setCaptured(null);
        setComment('');
      }
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [mode]);

  useEffect(() => {
    if (!toast) return;
    const t = setTimeout(() => setToast(null), 2500);
    return () => clearTimeout(t);
  }, [toast]);

  const cancel = () => {
    setMode('idle');
    setCaptured(null);
    setComment('');
  };

  const save = async () => {
    if (!captured || !comment.trim()) return;
    setSaving(true);
    try {
      const res = await fetch('/api/v1/local/feedback', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ...captured, comment: comment.trim() }),
      });
      if (!res.ok) throw new Error(await res.text());
      setToast('saved to llm/ui-feedback.md');
      setMode('idle');
      setCaptured(null);
      setComment('');
    } catch (e) {
      setToast(`error: ${e instanceof Error ? e.message : 'save failed'}`);
    } finally {
      setSaving(false);
    }
  };

  const hoverRect = hoveredEl?.getBoundingClientRect();

  return (
    <div data-dev-overlay>
      {mode === 'selecting' && hoverRect && (
        <div
          style={{
            position: 'fixed',
            top: hoverRect.top,
            left: hoverRect.left,
            width: hoverRect.width,
            height: hoverRect.height,
            border: '2px solid #ef4444',
            background: 'rgba(239, 68, 68, 0.08)',
            pointerEvents: 'none',
            zIndex: 9998,
          }}
        />
      )}

      {mode === 'selecting' && (
        <div
          style={{
            position: 'fixed',
            top: 12,
            left: '50%',
            transform: 'translateX(-50%)',
            background: '#0f172a',
            color: 'white',
            padding: '6px 12px',
            borderRadius: 6,
            fontSize: 13,
            zIndex: 10000,
            boxShadow: '0 2px 8px rgba(0,0,0,0.2)',
            fontFamily: 'system-ui, -apple-system, sans-serif',
          }}
        >
          Click any element to leave a comment. Esc to cancel.
        </div>
      )}

      {mode === 'idle' && (
        <button
          onClick={() => setMode('selecting')}
          title="Add UI feedback (dev only)"
          aria-label="Add UI feedback"
          style={{
            position: 'fixed',
            bottom: 16,
            right: 16,
            width: 44,
            height: 44,
            borderRadius: '50%',
            background: '#0f172a',
            color: 'white',
            border: 'none',
            cursor: 'pointer',
            fontSize: 18,
            zIndex: 9997,
            boxShadow: '0 2px 8px rgba(0,0,0,0.2)',
          }}
        >
          💬
        </button>
      )}

      {mode === 'modal' && captured && (
        <>
          <div
            onClick={cancel}
            style={{
              position: 'fixed',
              inset: 0,
              background: 'rgba(0,0,0,0.4)',
              zIndex: 9999,
            }}
          />
          <div
            style={{
              position: 'fixed',
              top: '50%',
              left: '50%',
              transform: 'translate(-50%, -50%)',
              background: 'white',
              color: '#0f172a',
              padding: 20,
              borderRadius: 8,
              width: 'min(520px, 90vw)',
              zIndex: 10001,
              boxShadow: '0 8px 32px rgba(0,0,0,0.25)',
              fontFamily: 'system-ui, -apple-system, sans-serif',
            }}
          >
            <div style={{ fontSize: 14, fontWeight: 600, marginBottom: 12 }}>UI feedback</div>
            <div style={{ fontSize: 12, color: '#475569', marginBottom: 4 }}>Route</div>
            <div style={{ fontSize: 13, fontFamily: 'monospace', marginBottom: 10 }}>{captured.route}</div>
            <div style={{ fontSize: 12, color: '#475569', marginBottom: 4 }}>Anchor</div>
            <div style={{ fontSize: 13, marginBottom: 10 }}>{captured.anchor}</div>
            <div style={{ fontSize: 12, color: '#475569', marginBottom: 4 }}>Selector</div>
            <div
              style={{
                fontSize: 12,
                fontFamily: 'monospace',
                color: '#64748b',
                marginBottom: 12,
                wordBreak: 'break-all',
              }}
            >
              {captured.selector}
            </div>
            <textarea
              autoFocus
              placeholder="What's wrong / what would you change?"
              value={comment}
              onChange={(e) => setComment(e.target.value)}
              onKeyDown={(e) => {
                if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') save();
              }}
              style={{
                width: '100%',
                minHeight: 100,
                padding: 8,
                border: '1px solid #cbd5e1',
                borderRadius: 4,
                fontSize: 13,
                fontFamily: 'inherit',
                resize: 'vertical',
                boxSizing: 'border-box',
                color: '#0f172a',
              }}
            />
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginTop: 12 }}>
              <span style={{ fontSize: 11, color: '#94a3b8' }}>⌘+Enter to save</span>
              <div style={{ display: 'flex', gap: 8 }}>
                <button
                  onClick={cancel}
                  style={{
                    padding: '6px 12px',
                    border: '1px solid #cbd5e1',
                    background: 'white',
                    color: '#0f172a',
                    borderRadius: 4,
                    cursor: 'pointer',
                    fontSize: 13,
                  }}
                >
                  Cancel
                </button>
                <button
                  onClick={save}
                  disabled={!comment.trim() || saving}
                  style={{
                    padding: '6px 12px',
                    border: 'none',
                    background: comment.trim() && !saving ? '#0f172a' : '#94a3b8',
                    color: 'white',
                    borderRadius: 4,
                    cursor: comment.trim() && !saving ? 'pointer' : 'not-allowed',
                    fontSize: 13,
                  }}
                >
                  {saving ? 'Saving…' : 'Save'}
                </button>
              </div>
            </div>
          </div>
        </>
      )}

      {toast && (
        <div
          style={{
            position: 'fixed',
            bottom: 80,
            right: 16,
            background: '#0f172a',
            color: 'white',
            padding: '8px 14px',
            borderRadius: 6,
            fontSize: 13,
            zIndex: 10002,
            boxShadow: '0 2px 8px rgba(0,0,0,0.2)',
            fontFamily: 'system-ui, -apple-system, sans-serif',
          }}
        >
          {toast}
        </div>
      )}
    </div>
  );
}
