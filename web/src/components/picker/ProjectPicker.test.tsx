import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { PickerExplainer, ProjectPicker, rowsFromSummaries } from './ProjectPicker';
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
      expect(target).toHaveAccessibleDescription(testCase.expectedCoverageLabel);
      expect(within(target).getByLabelText(testCase.expectedCoverageLabel)).toBeInTheDocument();

      fireEvent.change(search, { target: { value: testCase.searchQuery } });
      expect(screen.getAllByRole('link')).toEqual([target]);
      expect(target).toHaveAttribute('href', testCase.expectedHref);
    });
  }
});

describe('PickerExplainer fixture copy', () => {
  it('renders the revised copy on the production component path', () => {
    const explainer = { id: 'changes', open: true, hydrated: true, show: () => {}, hide: () => {} };
    render(<PickerExplainer explainer={explainer} destination="changes" />);
    const region = screen.getByRole('region');
    expect(region).toHaveTextContent(localReviewClarityFixture.copy.explainerIntro);
    expect(region).toHaveTextContent(localReviewClarityFixture.copy.explainerRecordedFile);
    expect(region).toHaveTextContent(localReviewClarityFixture.copy.explainerDestinationHome);
  });

  it('renders the map destination copy from the fixture', () => {
    const explainer = { id: 'map', open: true, hydrated: true, show: () => {}, hide: () => {} };
    render(<PickerExplainer explainer={explainer} destination="map" />);
    expect(screen.getByRole('region')).toHaveTextContent(localReviewClarityFixture.copy.explainerDestinationMap);
  });
});
