import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { useState } from 'react';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { parse } from 'yaml';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { SessionPicker } from './SessionPicker';
import type { ShareHierarchySession } from '@/lib/share/types';

interface PickerFixture {
  largeHistory: {
    projects: number;
    locationsPerProject: number;
    branchesPerLocation: number;
    sessionsPerBranch: number;
    unavailableEvery: number;
    expectedSessions: number;
    expectedInitialRailMeasurements: number;
  };
  interactions: {
    leaf: { sessionId: string; expectedBranchSelectable: number; expectedBranchTokens: number; projectLabel: string; locationLabel: string; branchLabel: string };
    disabled: { sessionId: string };
  };
}

const fixture = parse(readFileSync(resolve(process.cwd(), 'src/lib/share/testdata/session_picker.yaml'), 'utf8')) as PickerFixture;

function loadLargeHistory(): ShareHierarchySession[] {
  const config = fixture.largeHistory;
  const sessions: ShareHierarchySession[] = [];
  for (let project = 0; project < config.projects; project++) {
    for (let location = 0; location < config.locationsPerProject; location++) {
      for (let branch = 0; branch < config.branchesPerLocation; branch++) {
        for (let index = 0; index < config.sessionsPerBranch; index++) {
          const ordinal = sessions.length;
          sessions.push({
            id: `session-${project}-${location}-${branch}-${index}`,
            provider: 'claude-code', projectName: `project-${project}`, projectHash: `hash-${project}`,
            hostSlug: 'fixture-host', startTime: '2026-08-12T00:00:00Z', durationMins: 1,
            totalTokens: ordinal + 1, turnCount: 1, model: 'fixture-model',
            shareStatus: ordinal % config.unavailableEvery === 0 ? 'shared' : 'new', preview: `fixture prompt ${ordinal}`,
            locationLabel: `workspace-${project}-${location}`, repositoryLocationId: `location-${project}-${location}`,
            branch: `branch-${branch}`,
          });
        }
      }
    }
  }
  if (sessions.length !== config.expectedSessions) throw new Error(`session picker fixture generated ${sessions.length} sessions; expected ${config.expectedSessions}`);
  return sessions;
}

function MountedPicker({ sessions }: { sessions: ShareHierarchySession[] }) {
  const [selectedIds, setSelectedIds] = useState(new Set<string>());
  return <SessionPicker sessions={sessions} selectedIds={selectedIds} onSelectionChange={setSelectedIds} onNext={() => undefined} />;
}

describe('SessionPicker large-history interactions', () => {
  let rectSpy: ReturnType<typeof vi.spyOn>;
  let observerConstructions: number;

  beforeEach(() => {
    observerConstructions = 0;
    rectSpy = vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue({ x: 0, y: 0, top: 0, left: 0, right: 16, bottom: 16, width: 16, height: 16, toJSON: () => ({}) });
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => { callback(0); return 1; });
    vi.stubGlobal('cancelAnimationFrame', () => undefined);
    vi.stubGlobal('ResizeObserver', class ResizeObserver {
      constructor(_callback: ResizeObserverCallback) { observerConstructions++; }
      observe() {}
      disconnect() {}
      unobserve() {}
    });
  });

  afterEach(() => vi.unstubAllGlobals());

  it('preserves leaf and ancestor tri-state semantics without remeasuring the rail on selection changes', async () => {
    render(<MountedPicker sessions={loadLargeHistory()} />);
    const leaf = screen.getByRole('checkbox', { name: `select session ${fixture.interactions.leaf.sessionId}` });
    const project = screen.getByRole('checkbox', { name: fixture.interactions.leaf.projectLabel });
    const location = screen.getByRole('checkbox', { name: fixture.interactions.leaf.locationLabel });
    const locationSection = screen.getByRole('region', { name: 'repository location workspace-0-0' });
    const branch = within(locationSection).getByRole('checkbox', { name: fixture.interactions.leaf.branchLabel });
    const disabled = screen.getByRole('checkbox', { name: `select session ${fixture.interactions.disabled.sessionId}` });

    expect(disabled).toBeDisabled();
    expect(rectSpy).toHaveBeenCalledTimes(fixture.largeHistory.expectedInitialRailMeasurements);
    const measuredBeforeToggle = rectSpy.mock.calls.length;
    const observersBeforeToggle = observerConstructions;

    await userEvent.click(leaf);
    await waitFor(() => expect(leaf).toBeChecked());
    expect(project).toHaveAttribute('aria-checked', 'mixed');
    expect(location).toHaveAttribute('aria-checked', 'mixed');
    expect(branch).toHaveAttribute('aria-checked', 'mixed');
    expect(rectSpy).toHaveBeenCalledTimes(measuredBeforeToggle);
    expect(observerConstructions).toBe(observersBeforeToggle);

    await userEvent.click(branch);
    await waitFor(() => expect(branch).toBeChecked());
    expect(project).toHaveAttribute('aria-checked', 'mixed');
    expect(location).toHaveAttribute('aria-checked', 'mixed');
    expect(screen.getByText(`${fixture.interactions.leaf.expectedBranchSelectable} selected · ${fixture.interactions.leaf.expectedBranchTokens} tokens`)).toBeInTheDocument();
    expect(rectSpy).toHaveBeenCalledTimes(measuredBeforeToggle);
    expect(observerConstructions).toBe(observersBeforeToggle);
  });
});
