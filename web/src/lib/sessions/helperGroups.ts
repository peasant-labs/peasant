/**
 * Host-owned paging for one expanded helper group.
 *
 * Fairtrade's `HelperGroup` deliberately owns no fetch, page or scope state:
 * the host passes the loaded member rows and renders its paging controls in the
 * `memberFooter` slot. Every rendered group therefore owns ONE independent
 * paging state, and this module is the pure arithmetic behind it — clamping a
 * page into range, and reporting the window and previous/next availability that
 * the footer renders.
 */

/** Default member page size. Matches the server's member page default. */
export const DEFAULT_HELPER_MEMBER_LIMIT = 20;

export interface HelperMemberPaging {
  /** 1-based page number. */
  page: number;
  /** Page size, at least 1. */
  limit: number;
  /** Server-reported total members for the scope; 0 before the first page. */
  total: number;
}

export interface HelperMemberPageWindow {
  /** Zero-based index of the first row on this page. */
  start: number;
  /** Exclusive index after the last row on this page. */
  end: number;
  /** Number of pages at the current total and limit, at least 1. */
  pageCount: number;
  hasPrevious: boolean;
  hasNext: boolean;
}

function requirePositiveInteger(value: number, field: string): number {
  if (!Number.isSafeInteger(value) || value < 1) {
    throw new Error(
      `Helper member paging rejected ${field}=${String(value)} in helperGroups because paging values must be positive integers. No request was issued and no page was rendered. Use a page number and limit of at least 1, then retry.`,
    );
  }
  return value;
}

/** The first page for a group, before any member fetch has resolved. */
export function firstHelperMemberPage(limit = DEFAULT_HELPER_MEMBER_LIMIT): HelperMemberPaging {
  return { page: 1, limit: requirePositiveInteger(limit, 'limit'), total: 0 };
}

/** Move to one page, keeping the limit and current total. */
export function gotoHelperMemberPage(paging: HelperMemberPaging, page: number): HelperMemberPaging {
  return { ...paging, page: requirePositiveInteger(page, 'page') };
}

/**
 * Record a fetched page's authoritative total and the page the server actually
 * served. A scope whose membership shrank under a live update can return a
 * lower page than requested; the footer must follow the server rather than
 * claim a page it never loaded.
 */
export function withHelperMemberTotal(
  paging: HelperMemberPaging,
  served: { page: number; total: number },
): HelperMemberPaging {
  return {
    ...paging,
    page: requirePositiveInteger(served.page, 'served.page'),
    total: Number.isSafeInteger(served.total) && served.total >= 0 ? served.total : paging.total,
  };
}

/**
 * The window and navigation availability for a paging state. A positive page
 * beyond the last clamps to the last page, so a shrinking member set never
 * leaves the footer pointing past the data; a malformed page or limit is
 * refused rather than clamped.
 */
export function helperMemberPageWindow(paging: HelperMemberPaging): HelperMemberPageWindow {
  const limit = requirePositiveInteger(paging.limit, 'limit');
  const requested = requirePositiveInteger(paging.page, 'page');
  const pageCount = Math.max(1, Math.ceil(paging.total / limit));
  const page = Math.min(requested, pageCount);
  const start = (page - 1) * limit;
  const end = Math.min(start + limit, paging.total);
  return { start, end, pageCount, hasPrevious: page > 1, hasNext: page < pageCount };
}
