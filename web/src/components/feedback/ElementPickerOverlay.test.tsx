import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { PickerDirection, type ElementPickerAdapter, type PickedElementDraft } from '@/lib/feedback/types';
import { ElementPickerOverlay } from './ElementPickerOverlay';

interface FakeAdapterHarness {
  adapter: ElementPickerAdapter;
  stepMock: ReturnType<typeof vi.fn>;
  describeMock: ReturnType<typeof vi.fn>;
  isIgnoredMock: ReturnType<typeof vi.fn>;
  isTooLargeMock: ReturnType<typeof vi.fn>;
  labelForMock: ReturnType<typeof vi.fn>;
  sectionOfMock: ReturnType<typeof vi.fn>;
}

function createFakeAdapter(options?: {
  ignored?: Set<Element>;
  tooLarge?: Set<Element>;
  stepMap?: Map<string, Element | null>;
}): FakeAdapterHarness {
  const ignored = options?.ignored ?? new Set<Element>();
  const tooLarge = options?.tooLarge ?? new Set<Element>();
  const stepMap = options?.stepMap ?? new Map<string, Element | null>();

  const isIgnoredMock = vi.fn((el: Element) => ignored.has(el) || Boolean(el.closest('[data-feedback-widget]')));
  const isTooLargeMock = vi.fn((rect: Pick<DOMRect, 'width' | 'height'>) => {
    for (const el of tooLarge) {
      const r = el.getBoundingClientRect();
      if (r.width === rect.width && r.height === rect.height) return true;
    }
    return false;
  });
  const labelForMock = vi.fn((el: Element) => `${el.tagName.toLowerCase()}${el.id ? `#${el.id}` : ''}`);
  const sectionOfMock = vi.fn((el: Element) => el.closest('section')?.getAttribute('aria-label') ?? '');
  const describeMock = vi.fn((el: Element): PickedElementDraft => {
    const r = el.getBoundingClientRect();
    return {
      selector: el.id ? `#${el.id}` : el.tagName.toLowerCase(),
      label: labelForMock(el),
      section: sectionOfMock(el),
      textSnippet: (el.textContent ?? '').slice(0, 120),
      box: { top: r.top, left: r.left, width: r.width, height: r.height },
    };
  });
  const stepMock = vi.fn((current: Element | null, direction: PickerDirection) => {
    const key = `${current ? current.id || current.tagName.toLowerCase() : 'none'}:${direction.wire}`;
    return stepMap.get(key) ?? null;
  });

  return {
    adapter: {
      isIgnoredElement: isIgnoredMock,
      isTooLargeRect: isTooLargeMock,
      labelFor: labelForMock,
      sectionOf: sectionOfMock,
      describe: describeMock,
      step: stepMock,
    },
    stepMock,
    describeMock,
    isIgnoredMock,
    isTooLargeMock,
    labelForMock,
    sectionOfMock,
  };
}

function mockRect(el: Element, rect: { top: number; left: number; width: number; height: number }) {
  vi.spyOn(el, 'getBoundingClientRect').mockReturnValue({
    top: rect.top,
    left: rect.left,
    width: rect.width,
    height: rect.height,
    right: rect.left + rect.width,
    bottom: rect.top + rect.height,
    x: rect.left,
    y: rect.top,
    toJSON: () => ({}),
  } as DOMRect);
}

describe('ElementPickerOverlay', () => {
  let rafQueue: FrameRequestCallback[];

  beforeEach(() => {
    rafQueue = [];
    vi.spyOn(window, 'requestAnimationFrame').mockImplementation((cb) => {
      rafQueue.push(cb);
      return rafQueue.length;
    });
    vi.spyOn(window, 'cancelAnimationFrame').mockImplementation(() => {});
  });

  afterEach(() => {
    cleanup();
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  const flushRaf = () => {
    const pending = rafQueue.splice(0);
    act(() => {
      for (const cb of pending) cb(0);
    });
  };

  it('renders nothing when inactive and portals 4 scrims + ring + status bar when active', () => {
    const { adapter } = createFakeAdapter();
    const { rerender } = render(
      <ElementPickerOverlay active={false} adapter={adapter} onConfirm={vi.fn()} onCancel={vi.fn()} />,
    );
    expect(document.body.querySelector('[data-feedback-widget]')).toBeNull();

    rerender(<ElementPickerOverlay active adapter={adapter} onConfirm={vi.fn()} onCancel={vi.fn()} />);
    const root = document.body.querySelector('[data-feedback-widget]');
    expect(root).not.toBeNull();
    expect(screen.getByText('pick an element · enter to choose · esc to cancel')).toBeInTheDocument();
    expect(root?.querySelectorAll('.fbw-picker-scrim')).toHaveLength(1);
    expect(root?.querySelector('.fbw-picker-ring')).toBeNull();
  });

  it('tracks pointermove with 4 scrim rects, amber ring, and polite live-region announcement while skipping ignored/too-large targets', () => {
    const section = document.createElement('section');
    section.setAttribute('aria-label', 'sessions');
    const card = document.createElement('button');
    card.id = 'target-card';
    card.textContent = 'open session';
    const wrapper = document.createElement('div');
    wrapper.id = 'too-big';
    const ignored = document.createElement('div');
    ignored.id = 'ignored-el';

    section.append(card, wrapper, ignored);
    document.body.appendChild(section);

    mockRect(card, { top: 100, left: 80, width: 200, height: 48 });
    mockRect(wrapper, { top: 0, left: 0, width: 1200, height: 900 });
    mockRect(ignored, { top: 20, left: 20, width: 40, height: 40 });

    const { adapter } = createFakeAdapter({
      ignored: new Set([ignored]),
      tooLarge: new Set([wrapper]),
    });

    render(<ElementPickerOverlay active adapter={adapter} onConfirm={vi.fn()} onCancel={vi.fn()} />);

    fireEvent.pointerMove(card);
    const root = document.body.querySelector('[data-feedback-widget]')!;
    expect(root.querySelectorAll('.fbw-picker-scrim')).toHaveLength(4);
    const ring = root.querySelector('.fbw-picker-ring') as HTMLElement | null;
    expect(ring).not.toBeNull();
    expect(ring?.style.top).toBe('100px');
    expect(ring?.style.left).toBe('80px');
    expect(ring?.style.width).toBe('200px');
    expect(ring?.style.height).toBe('48px');

    const live = root.querySelector('[aria-live="polite"]');
    expect(live?.textContent).toContain('button#target-card');

    // Moving onto a too-large wrapper clears the target rather than highlighting it.
    fireEvent.pointerMove(wrapper);
    expect(root.querySelector('.fbw-picker-ring')).toBeNull();

    // Moving onto an ignored element stays ignored.
    fireEvent.pointerMove(ignored);
    expect(root.querySelector('.fbw-picker-ring')).toBeNull();
  });

  it('intercepts capture-phase click and confirms without activating page listeners', () => {
    const pageClick = vi.fn();
    const target = document.createElement('button');
    target.id = 'pick-me';
    target.textContent = 'click me';
    target.addEventListener('click', pageClick);
    document.body.appendChild(target);
    mockRect(target, { top: 40, left: 50, width: 120, height: 36 });

    const { adapter } = createFakeAdapter();
    const onConfirm = vi.fn();
    render(<ElementPickerOverlay active adapter={adapter} onConfirm={onConfirm} onCancel={vi.fn()} />);

    fireEvent.click(target);
    expect(pageClick).not.toHaveBeenCalled();
    expect(onConfirm).toHaveBeenCalledTimes(1);
    expect(onConfirm.mock.calls[0][0]).toBe(target);
    expect(onConfirm.mock.calls[0][1].selector).toBe('#pick-me');
  });

  it('navigates via adapter.step on arrow/Tab keys and confirms with Enter/Space or cancels with Escape', () => {
    const parentEl = document.createElement('div');
    parentEl.id = 'parent-box';
    const childEl = document.createElement('button');
    childEl.id = 'child-btn';
    const siblingEl = document.createElement('button');
    siblingEl.id = 'sibling-btn';
    const landmarkEl = document.createElement('nav');
    landmarkEl.id = 'main-nav';
    parentEl.append(childEl, siblingEl);
    document.body.append(parentEl, landmarkEl);

    mockRect(parentEl, { top: 10, left: 10, width: 300, height: 100 });
    mockRect(childEl, { top: 20, left: 20, width: 100, height: 36 });
    mockRect(siblingEl, { top: 20, left: 130, width: 100, height: 36 });
    mockRect(landmarkEl, { top: 0, left: 0, width: 200, height: 600 });

    const stepMap = new Map<string, Element | null>([
      [`none:${PickerDirection.NextLandmark.wire}`, landmarkEl],
      [`main-nav:${PickerDirection.PrevLandmark.wire}`, parentEl],
      [`parent-box:${PickerDirection.Child.wire}`, childEl],
      [`child-btn:${PickerDirection.NextSibling.wire}`, siblingEl],
      [`sibling-btn:${PickerDirection.PrevSibling.wire}`, childEl],
      [`child-btn:${PickerDirection.Parent.wire}`, parentEl],
    ]);

    const { adapter, stepMock } = createFakeAdapter({ stepMap });
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    render(<ElementPickerOverlay active adapter={adapter} onConfirm={onConfirm} onCancel={onCancel} />);

    fireEvent.keyDown(document, { key: 'Tab' });
    expect(stepMock).toHaveBeenLastCalledWith(null, PickerDirection.NextLandmark, document);

    fireEvent.keyDown(document, { key: 'Tab', shiftKey: true });
    expect(stepMock).toHaveBeenLastCalledWith(landmarkEl, PickerDirection.PrevLandmark, document);

    fireEvent.keyDown(document, { key: 'ArrowDown' });
    expect(stepMock).toHaveBeenLastCalledWith(parentEl, PickerDirection.Child, document);

    fireEvent.keyDown(document, { key: 'ArrowRight' });
    expect(stepMock).toHaveBeenLastCalledWith(childEl, PickerDirection.NextSibling, document);

    fireEvent.keyDown(document, { key: 'ArrowLeft' });
    expect(stepMock).toHaveBeenLastCalledWith(siblingEl, PickerDirection.PrevSibling, document);

    fireEvent.keyDown(document, { key: 'ArrowUp' });
    expect(stepMock).toHaveBeenLastCalledWith(childEl, PickerDirection.Parent, document);

    fireEvent.keyDown(document, { key: 'Enter' });
    expect(onConfirm).toHaveBeenCalledTimes(1);
    expect(onConfirm.mock.calls[0][0]).toBe(parentEl);

    fireEvent.keyDown(document, { key: ' ' });
    expect(onConfirm).toHaveBeenCalledTimes(2);

    fireEvent.keyDown(document, { key: 'Escape' });
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it('re-measures the highlighted element on scroll and resize via rAF, and cleans up all listeners on deactivate', () => {
    const el = document.createElement('div');
    el.id = 'moving';
    document.body.appendChild(el);
    let top = 50;
    vi.spyOn(el, 'getBoundingClientRect').mockImplementation(
      () =>
        ({
          top,
          left: 30,
          width: 140,
          height: 40,
          right: 170,
          bottom: top + 40,
          x: 30,
          y: top,
          toJSON: () => ({}),
        }) as DOMRect,
    );

    const { adapter, isIgnoredMock, stepMock } = createFakeAdapter();
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    const { rerender } = render(
      <ElementPickerOverlay active adapter={adapter} onConfirm={onConfirm} onCancel={onCancel} />,
    );

    fireEvent.pointerMove(el);
    const ring = () => document.body.querySelector('.fbw-picker-ring') as HTMLElement | null;
    expect(ring()?.style.top).toBe('50px');

    top = 120;
    fireEvent.scroll(window);
    flushRaf();
    expect(ring()?.style.top).toBe('120px');

    top = 180;
    fireEvent.resize(window);
    flushRaf();
    expect(ring()?.style.top).toBe('180px');

    rerender(<ElementPickerOverlay active={false} adapter={adapter} onConfirm={onConfirm} onCancel={onCancel} />);
    isIgnoredMock.mockClear();
    stepMock.mockClear();

    fireEvent.pointerMove(el);
    fireEvent.click(el);
    fireEvent.keyDown(document, { key: 'Escape' });
    fireEvent.keyDown(document, { key: 'ArrowDown' });
    expect(isIgnoredMock).not.toHaveBeenCalled();
    expect(stepMock).not.toHaveBeenCalled();
    expect(onConfirm).not.toHaveBeenCalled();
    expect(onCancel).not.toHaveBeenCalled();
  });
});
