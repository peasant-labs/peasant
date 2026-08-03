import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act, renderHook } from '@testing-library/react';
import {
  useTourState,
  readTourRecord,
  TOUR_STORAGE_KEY,
} from './useTourState';

const STEP_COUNT = 5;

function render() {
  return renderHook(() => useTourState({ stepCount: STEP_COUNT }));
}

describe('useTourState', () => {
  beforeEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it('starts inactive at step 0 with no completion record', () => {
    const { result } = render();
    expect(result.current.active).toBe(false);
    expect(result.current.index).toBe(0);
    expect(result.current.completed).toBe(false);
    expect(result.current.isFirst).toBe(true);
    expect(result.current.isLast).toBe(false);
  });

  it('start() activates the tour from the first step', () => {
    const { result } = render();
    act(() => result.current.start());
    expect(result.current.active).toBe(true);
    expect(result.current.index).toBe(0);
  });

  it('next() advances through steps and tracks isLast', () => {
    const { result } = render();
    act(() => result.current.start());

    for (let i = 1; i < STEP_COUNT; i++) {
      act(() => result.current.next());
      expect(result.current.index).toBe(i);
    }
    expect(result.current.isLast).toBe(true);
    expect(result.current.active).toBe(true);
  });

  it('prev() moves back but never below the first step', () => {
    const { result } = render();
    act(() => result.current.start());
    act(() => result.current.next());
    act(() => result.current.next());
    expect(result.current.index).toBe(2);

    act(() => result.current.prev());
    expect(result.current.index).toBe(1);

    act(() => result.current.prev());
    act(() => result.current.prev());
    expect(result.current.index).toBe(0);
  });

  it('goTo() clamps to the valid range', () => {
    const { result } = render();
    act(() => result.current.start());

    act(() => result.current.goTo(99));
    expect(result.current.index).toBe(STEP_COUNT - 1);

    act(() => result.current.goTo(-5));
    expect(result.current.index).toBe(0);
  });

  it('next() on the last step completes and persists a record', () => {
    const { result } = render();
    act(() => result.current.start());
    act(() => result.current.goTo(STEP_COUNT - 1));

    act(() => result.current.next());

    expect(result.current.active).toBe(false);
    expect(result.current.completed).toBe(true);
    const record = readTourRecord();
    expect(record).not.toBeNull();
    expect(typeof record?.completedAt).toBe('string');
  });

  it('skip() ends the tour and persists completion', () => {
    const { result } = render();
    act(() => result.current.start());
    act(() => result.current.next());

    act(() => result.current.skip());

    expect(result.current.active).toBe(false);
    expect(result.current.completed).toBe(true);
    expect(readTourRecord()).not.toBeNull();
  });

  it('complete() ends the tour and persists completion', () => {
    const { result } = render();
    act(() => result.current.start());

    act(() => result.current.complete());

    expect(result.current.active).toBe(false);
    expect(result.current.completed).toBe(true);
    expect(readTourRecord()).not.toBeNull();
  });

  it('does not re-trigger: a fresh hook hydrates completed=true from storage', () => {
    // First session: complete the tour.
    const first = render();
    act(() => first.result.current.start());
    act(() => first.result.current.skip());
    expect(readTourRecord()).not.toBeNull();

    // Second session (e.g. a page reload): a brand-new hook instance.
    const second = render();
    expect(second.result.current.completed).toBe(true);
    // Auto-start logic in the provider keys off `completed`, so this is the
    // signal that prevents the tour from showing again.
    expect(second.result.current.active).toBe(false);
  });

  it('persists under the documented localStorage key as JSON', () => {
    const { result } = render();
    act(() => result.current.start());
    act(() => result.current.complete());

    const raw = localStorage.getItem(TOUR_STORAGE_KEY);
    expect(raw).toBeTruthy();
    const parsed = JSON.parse(raw as string);
    expect(parsed).toHaveProperty('completedAt');
  });

  it('tolerates a corrupt localStorage value (treats it as not completed)', () => {
    localStorage.setItem(TOUR_STORAGE_KEY, '{not json');
    const { result } = render();
    expect(result.current.completed).toBe(false);
    expect(readTourRecord()).toBeNull();
  });
});
