import { useState } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { CodeMap } from '@peasant-labs/fairtrade/graph';
import { adaptCodeMap } from '@/lib/adapters/map';
import type { MapZoom } from '@/components/map';
import { GRAPH_WITH_FILE, PROJECT } from '../lib/test-fixtures';

const payload = adaptCodeMap(GRAPH_WITH_FILE);

function CodeMapHarness() {
  const [zoom, setZoom] = useState<MapZoom>({ level: 'package', expanded: new Set() });
  const [selectedId, setSelectedId] = useState<string | null>(null);

  return (
    <CodeMap
      payload={payload}
      zoom={zoom}
      onZoomChange={(next) => {
        setZoom({ level: next.level, expanded: new Set(next.expanded) });
      }}
      selectedId={selectedId}
      onSelect={(id) => setSelectedId(id)}
      height={520}
      ariaLabel={`Code map of ${PROJECT}`}
    />
  );
}

describe('CodeMap real package composition', () => {
  it('renders adapted map topology with real CodeMap controls and search-driven grain state', async () => {
    const user = userEvent.setup();
    const raf = vi.spyOn(window, 'requestAnimationFrame').mockImplementation((cb) => {
      cb(0);
      return 0;
    });

    try {
      const { container } = render(<CodeMapHarness />);

      const map = screen.getByRole('application', { name: `Code map of ${PROJECT}` });
      expect(map).toHaveClass('mc');
      expect(container.querySelectorAll('.mc-edge')).toHaveLength(1);
      expect(screen.getByRole('button', { name: /cmd: folder/ })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: /ingest: folder/ })).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: /pipeline\.go: file/ })).not.toBeInTheDocument();

      await user.click(screen.getByRole('radio', { name: 'overview' }));
      expect(screen.getByRole('radio', { name: 'overview' })).toHaveAttribute('aria-checked', 'true');
      expect(screen.queryByRole('button', { name: /ingest: folder/ })).not.toBeInTheDocument();

      const search = screen.getByRole('textbox', { name: 'find a node' });
      await user.type(search, 'pipeline');
      const option = await screen.findByRole('option', { name: /internal\/ingest\/pipeline\.go/ });
      await user.click(option);

      await waitFor(() => {
        expect(screen.getByRole('radio', { name: 'files' })).toHaveAttribute('aria-checked', 'true');
      });
      expect(screen.getByRole('button', { name: /pipeline\.go: file/ })).toHaveAttribute(
        'aria-pressed',
        'true',
      );
      expect(search).toHaveValue('');
    } finally {
      raf.mockRestore();
    }
  });
});
