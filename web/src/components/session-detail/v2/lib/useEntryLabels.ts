'use client';

import { useCallback, useEffect, useMemo, useState } from 'react';
import {
  fetchAnnotationTypes,
  fetchSessionAnnotations,
  entryApplicableTypes,
  type AnnotationType,
  type AnnotationSummary,
} from '@/lib/api/annotations';
import type { SavedTurnLabel } from '../canvas/TurnLabelPopover';

export interface UseEntryLabelsResult {
  /** Annotation types whose allowedTargetKinds includes "entry". */
  entryTypes: AnnotationType[];
  /** Saved + optimistic labels keyed by entry index (turn.index). */
  labelsByEntry: Map<number, SavedTurnLabel[]>;
  /** Merge a freshly-saved label optimistically. */
  addLabel: (label: SavedTurnLabel) => void;
}

/**
 * Map a backend AnnotationSummary to a SavedTurnLabel. Returns null for
 * annotations that do not target a single entry (no targetEntryIndex).
 */
export function summaryToTurnLabel(a: AnnotationSummary): SavedTurnLabel | null {
  if (a.targetKind !== 'entry' || a.targetEntryIndex == null) return null;
  return {
    entryIndex: a.targetEntryIndex,
    typeId: a.typeId,
    typeName: a.typeName,
    value: a.value,
    id: a.id,
  };
}

/**
 * Group backend entry annotations into a Map<entryIndex, SavedTurnLabel[]>.
 * Non-entry annotations (session/project/meta) are dropped.
 */
export function groupEntryAnnotations(
  annotations: AnnotationSummary[],
): Map<number, SavedTurnLabel[]> {
  const map = new Map<number, SavedTurnLabel[]>();
  for (const a of annotations) {
    const label = summaryToTurnLabel(a);
    if (!label) continue;
    const arr = map.get(label.entryIndex) ?? [];
    arr.push(label);
    map.set(label.entryIndex, arr);
  }
  return map;
}

/**
 * Loads entry-applicable annotation types and existing entry-level annotations
 * for a session via REST, exposing a merged label map. Newly-saved labels are
 * added optimistically; the WebSocket annotations channel does not yet emit
 * entry-axis updates, so REST is the source of truth on load.
 */
export function useEntryLabels(sessionId: string): UseEntryLabelsResult {
  const [allTypes, setAllTypes] = useState<AnnotationType[]>([]);
  const [serverAnnotations, setServerAnnotations] = useState<AnnotationSummary[]>([]);
  // Optimistic labels created this session, not yet (or in addition to) reloaded.
  const [optimistic, setOptimistic] = useState<SavedTurnLabel[]>([]);

  useEffect(() => {
    let cancelled = false;
    // Types and annotations are independent; failures are non-fatal (the
    // affordance simply hides when no entry types load).
    fetchAnnotationTypes()
      .then((types) => {
        if (!cancelled) setAllTypes(types);
      })
      .catch(() => {
        if (!cancelled) setAllTypes([]);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    let cancelled = false;
    if (!sessionId) return;
    fetchSessionAnnotations(sessionId)
      .then((rows) => {
        if (!cancelled) setServerAnnotations(rows);
      })
      .catch(() => {
        if (!cancelled) setServerAnnotations([]);
      });
    return () => {
      cancelled = true;
    };
  }, [sessionId]);

  const entryTypes = useMemo(() => entryApplicableTypes(allTypes), [allTypes]);

  const labelsByEntry = useMemo(() => {
    const map = groupEntryAnnotations(serverAnnotations);
    // Merge optimistic labels, skipping any already present by DB id.
    for (const label of optimistic) {
      const arr = map.get(label.entryIndex) ?? [];
      if (label.id && arr.some((l) => l.id === label.id)) continue;
      arr.push(label);
      map.set(label.entryIndex, arr);
    }
    return map;
  }, [serverAnnotations, optimistic]);

  const addLabel = useCallback((label: SavedTurnLabel) => {
    setOptimistic((prev) => [...prev, label]);
  }, []);

  return { entryTypes, labelsByEntry, addLabel };
}
