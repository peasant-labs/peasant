/**
 * Cases behind PushStep.privacy.test.tsx: each license the endpoint can report
 * and the exact prose phrase + submit-button label the Share step must show.
 * Data, not an inline table, so a widened license menu adds one row here.
 */
export interface LicenseNoticeCase {
  name: string;
  /** The wire license id GET /api/v1/config/publish returns ('' = no license). */
  license: string;
  /** The lowercase phrase shown in the idle-step notice line. */
  phrase: string;
  /** The capitalized submit-button label. */
  buttonLabel: string;
}

export const LICENSE_NOTICE_CASES: readonly LicenseNoticeCase[] = [
  { name: 'cc-by', license: 'CC-BY-4.0', phrase: 'publish under CC BY 4.0', buttonLabel: 'Publish under CC BY 4.0' },
  { name: 'cc-by-sa', license: 'CC-BY-SA-4.0', phrase: 'publish under CC BY-SA 4.0', buttonLabel: 'Publish under CC BY-SA 4.0' },
  { name: 'cc0', license: 'CC0-1.0', phrase: 'publish under CC0 1.0', buttonLabel: 'Publish under CC0 1.0' },
  { name: 'no-license', license: '', phrase: 'publish without a license', buttonLabel: 'Publish without a license' },
];
