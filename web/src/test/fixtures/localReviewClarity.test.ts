import { describe, expect, it } from 'vitest';
import {
  loadLocalReviewClarityFixture,
  localReviewClarityFixture,
  localReviewClarityFixtureSource,
} from './localReviewClarity';

describe('local review clarity fixture', () => {
  it('loads the exact mounted-surface and outcome matrices', () => {
    expect(localReviewClarityFixture.pickerCases).toHaveLength(2);
    expect(localReviewClarityFixture.outcomeCases).toHaveLength(3);
  });

  it('rejects unknown fields and trailing YAML documents', () => {
    const withUnknownField = localReviewClarityFixtureSource.replace(
      '    surface: home',
      '    surface: home\n    unexpected: true',
    );
    expect(() => loadLocalReviewClarityFixture(withUnknownField)).toThrow(/unknown fields/);
    expect(() => loadLocalReviewClarityFixture(`${localReviewClarityFixtureSource}\n---\n{}`)).toThrow(
      /exactly one YAML document/,
    );
  });

  it('rejects a fixture that drops a required outcome state', () => {
    const failedCaseStart = localReviewClarityFixtureSource.indexOf(
      '\n  - name: mounted failed outcome exposes heuristic help',
    );
    expect(failedCaseStart).toBeGreaterThan(0);
    expect(() => loadLocalReviewClarityFixture(
      `${localReviewClarityFixtureSource.slice(0, failedCaseStart)}\n`,
    )).toThrow(/exactly 3 required cases/);
  });
});
