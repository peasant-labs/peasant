import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { ProjectPicker, rowsFromSummaries } from './ProjectPicker';
import {
  localReviewClarityFixture,
  makeClarityProjectSummaries,
} from '@/test/fixtures/localReviewClarity';

describe('ProjectPicker review clarity', () => {
  afterEach(() => cleanup());

  for (const testCase of localReviewClarityFixture.pickerCases) {
    it(testCase.name, () => {
      const projects = makeClarityProjectSummaries(testCase);
      render(
        <ProjectPicker
          rows={rowsFromSummaries({ projects })}
          destination={testCase.destination}
        />,
      );

      const search = screen.getByRole('searchbox', {
        name: localReviewClarityFixture.copy.searchAccessibleName,
      });
      expect(search).toHaveAttribute('placeholder', localReviewClarityFixture.copy.searchPlaceholder);
      const searchIcon = search.closest('.input-ico')?.querySelector('svg');
      expect(searchIcon).toBeInTheDocument();
      expect(searchIcon).toHaveAttribute('aria-hidden', 'true');

      const coverageHelp = screen.getByRole('button', {
        name: localReviewClarityFixture.copy.coverageHelpName,
      });
      expect(coverageHelp).toHaveTextContent(localReviewClarityFixture.copy.coverageVisibleLabel);
      expect(screen.queryByText('Files with AI')).not.toBeInTheDocument();
      fireEvent.focus(coverageHelp);
      const coverageTooltip = screen.getByRole('tooltip');
      expect(coverageTooltip).toHaveTextContent(localReviewClarityFixture.copy.coverageHelpText);
      expect(coverageHelp).toHaveAttribute('aria-describedby', coverageTooltip.id);

      const target = screen.getByRole('link', { name: testCase.expectedLinkName });
      expect(target).toHaveAttribute('href', testCase.expectedHref);
      expect(within(target).getByLabelText(testCase.expectedCoverageLabel)).toBeInTheDocument();

      fireEvent.change(search, { target: { value: testCase.searchQuery } });
      expect(screen.getAllByRole('link')).toEqual([target]);
      expect(target).toHaveAttribute('href', testCase.expectedHref);
    });
  }
});
