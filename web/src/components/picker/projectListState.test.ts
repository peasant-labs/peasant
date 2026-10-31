import { describe, expect, it } from 'vitest';
import { projectListState } from './projectListState';
import { PROJECT_VIEWER_STATE_FIXTURES } from './projectViewerStateFixtures';

describe('projectListState', () => {
  for (const testCase of PROJECT_VIEWER_STATE_FIXTURES) {
    it(testCase.name, () => {
      expect(projectListState(testCase.summary)).toBe(testCase.expectedState);
    });
  }
});
