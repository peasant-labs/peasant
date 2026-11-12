import { describe, expect, it } from 'vitest';
import { parseKickstartSession, type LocalSettingValue } from './model';
import { conflictValue, fieldByKey, structuredLines } from './view';
import { loadCases, loadModels } from '@/test/fixtures/kickstart';

/**
 * How one side of a conflict row is written out, played from a fixture: a
 * scalar is a phrase, an object or a list is readable lines in the server's
 * order.
 */

const PATH = 'src/lib/kickstart/testdata/structured_values.yaml';
const models = loadModels();

describe('a conflict value', () => {
  for (const entry of loadCases(PATH)) {
    it(entry.name, () => {
      const value = entry.value as LocalSettingValue;
      const field =
        typeof entry.model === 'string' && typeof entry.field === 'string'
          ? fieldByKey(parseKickstartSession(models[entry.model], PATH), entry.field)?.field
          : undefined;
      if (typeof entry.field === 'string') expect(field, `${entry.name}: the model serves the field ${entry.field}`).toBeDefined();
      const shown = conflictValue(value, field);
      if (Array.isArray(entry.lines)) {
        expect(shown.phrase).toBeNull();
        expect(shown.lines).toEqual(entry.lines);
        expect(structuredLines(value as Record<string, unknown>)).toEqual(entry.lines);
      } else {
        expect(shown.phrase).toBe(entry.phrase);
        expect(shown.lines).toEqual([]);
      }
    });
  }
});
