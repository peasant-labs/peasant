'use client';

import { ConnectionFaultNotice } from '@/components/ConnectionFaultNotice';
import { LocalOfflineNotice } from '@/components/LocalOfflineNotice';

/**
 * The page-column notices, in one slot above the page.
 *
 * They never stack: a stopped app is the offline notice's story, and the
 * connection-fault banner stands down while the socket is down. Only one of the
 * two can be on screen at a time.
 */
export function PageNotices() {
  return (
    <>
      <ConnectionFaultNotice />
      <LocalOfflineNotice />
    </>
  );
}
