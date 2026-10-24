import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import NotFound, { metadata } from './not-found';

afterEach(cleanup);

describe('not-found page', () => {
  it('names the miss as the page heading and links back home', () => {
    render(<NotFound />);
    expect(screen.getByRole('heading', { level: 1, name: 'page not found' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'go to home' })).toHaveAttribute('href', '/');
    expect(metadata.title).toBe('page not found | peasant');
  });
});
