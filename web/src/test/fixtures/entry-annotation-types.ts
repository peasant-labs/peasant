/**
 * Entry-level AnnotationType fixtures — the per-turn labeling affordance's
 * type registry. Mirrors the real rows seeded server-side:
 *   - CUSTOM_LABEL_TYPE:      internal/store/schema_v36.go (user.custom_label)
 *   - FRUSTRATION_SIGNAL_TYPE: internal/store/schema.go V18 (quality.frustration_signal)
 *   - TURN_OUTCOME_TYPE / TURN_FLAG_TYPE: internal/store/schema_v39.go
 *     (quality.turn_outcome / quality.turn_flag)
 *
 * Shared by TurnLabelPopover.test.tsx so a schema change to the fixture shape
 * updates one file, not every test that needs a type object.
 */
import type { AnnotationType } from '@/lib/api/annotations';

/** A free-text (described, no permissible values) entry type. */
export const CUSTOM_LABEL_TYPE: AnnotationType = {
  typeId: 'user.custom_label',
  version: 1,
  displayName: 'Custom label',
  description: 'A user-authored free-text label on a single turn.',
  family: 'turn_metadata',
  class: 'metadata',
  valueDomain: { kind: 'described', datatype: 'text' },
  status: 'active',
  origin: 'user',
  allowedTargetKinds: ['entry'],
};

/** An enumerated system-classifier entry type (2 permissible values). */
export const FRUSTRATION_SIGNAL_TYPE: AnnotationType = {
  typeId: 'quality.frustration_signal',
  version: 1,
  displayName: 'Frustration Signal',
  family: 'turn_quality',
  class: 'quality',
  valueDomain: {
    kind: 'enumerated',
    datatype: 'text',
    permissibleValues: ['detected', 'not_detected'],
  },
  status: 'active',
  origin: 'system',
  allowedTargetKinds: ['entry'],
};

/** The primary labeling modal's outcome axis: good/neutral/bad. */
export const TURN_OUTCOME_TYPE: AnnotationType = {
  typeId: 'quality.turn_outcome',
  version: 1,
  displayName: 'Turn outcome',
  description: 'A human verdict on a single turn: good, neutral, or bad.',
  family: 'turn_quality',
  class: 'quality',
  valueDomain: {
    kind: 'enumerated',
    datatype: 'text',
    permissibleValues: ['good', 'neutral', 'bad'],
  },
  status: 'active',
  origin: 'user',
  allowedTargetKinds: ['entry'],
};

/** The primary labeling modal's flag axis: an optional friction tag. */
export const TURN_FLAG_TYPE: AnnotationType = {
  typeId: 'quality.turn_flag',
  version: 1,
  displayName: 'Turn flag',
  description: 'An optional friction tag on a single turn.',
  family: 'turn_quality',
  class: 'quality',
  valueDomain: {
    kind: 'enumerated',
    datatype: 'text',
    permissibleValues: ['none', 'error', 'retry_loop', 'revert', 'highlight'],
  },
  status: 'active',
  origin: 'user',
  allowedTargetKinds: ['entry'],
};
