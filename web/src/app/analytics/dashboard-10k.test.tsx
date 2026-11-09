import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, within } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import type { ComponentProps, ReactNode } from 'react';
import { parse } from 'yaml';

// The analytics surface draws charts through Recharts. This gate asserts the
// section composition and the values sourced from the precomputed summary, not
// chart geometry, so the chart primitives render their children only.
vi.mock('recharts', () => {
  const Passthrough = ({ children }: { children?: ReactNode }) => <div>{children}</div>;
  const Blank = () => null;
  return {
    ResponsiveContainer: Passthrough,
    BarChart: Passthrough,
    ComposedChart: Passthrough,
    XAxis: Blank,
    YAxis: Blank,
    CartesianGrid: Blank,
    Tooltip: Blank,
    Bar: Blank,
    Line: Blank,
    Area: Blank,
  };
});

import {
  PROJECT_OVERVIEW_SECTION_KEYS,
  ProjectOverview,
} from '@peasant-labs/fairtrade/analytics';

interface SectionCase {
  name: string;
  section: string;
  label: string;
}

interface DashboardManifest {
  requiredNames: string[];
  sections: SectionCase[];
  page: number;
  perPage: number;
  total: number;
  expected: {
    totalSessions: number;
    contributorId: string;
    contributorSessions: number;
    outcomeTotal: number;
  };
}

interface DashboardFixture {
  page: number;
  perPage: number;
  total: number;
  summary: Record<string, unknown>;
}

const manifest = parse(
  readFileSync(resolve(process.cwd(), 'src/app/testdata/dashboard_10k.manifest.yaml'), 'utf8'),
) as DashboardManifest;
const fixture = parse(
  readFileSync(resolve(process.cwd(), 'src/app/testdata/dashboard_10k.yaml'), 'utf8'),
) as DashboardFixture;

afterEach(cleanup);

describe('the 10k dashboard at page 1 of 25', () => {
  it('names one case for every section and no section without a case', () => {
    const names = manifest.sections.map((section) => section.name);
    expect([...names].sort()).toEqual([...manifest.requiredNames].sort());
    expect(names.length).toBe(new Set(names).size);
    expect(manifest.sections.map((section) => section.section).sort()).toEqual(
      [...PROJECT_OVERVIEW_SECTION_KEYS].sort(),
    );
  });

  it('pages the summary at page 1 of 25 over the 10k library', () => {
    expect(fixture.page).toBe(manifest.page);
    expect(fixture.perPage).toBe(manifest.perPage);
    expect(fixture.total).toBe(manifest.total);
    expect(manifest.page).toBe(1);
    expect(manifest.perPage).toBe(25);
    expect(manifest.total).toBe(manifest.expected.totalSessions);
  });

  it('renders every dashboard section from the precomputed summary', () => {
    const payload = { analytics: fixture.summary } as unknown as ComponentProps<
      typeof ProjectOverview
    >['payload'];
    render(<ProjectOverview payload={payload} contributorLimit={10} />);

    for (const section of manifest.sections) {
      const chip = screen.getByRole('button', { name: `${section.label} section` });
      expect(chip).toHaveAttribute('aria-pressed', 'true');
    }

    const table = screen.getByRole('table');
    expect(within(table).getByText(manifest.expected.contributorId)).toBeTruthy();
    expect(within(table).getByText(String(manifest.expected.contributorSessions))).toBeTruthy();
    expect(screen.getAllByText(String(manifest.expected.totalSessions)).length).toBeGreaterThan(0);
  });
});
