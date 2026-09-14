/**
 * Maps the effective push license (from GET /api/v1/config/publish) to the
 * words the Share submit step shows before a person publishes. The prose phrase
 * is lowercase to match the consent-control wording used across Peasant's
 * surfaces; the button label capitalizes it as an action.
 *
 * The keys are the wire license ids the endpoint reports. Empty means no
 * license is attached. An unknown, non-empty id degrades to the no-license
 * wording rather than crashing, so a widened license menu never renders a blank
 * control while the web copy catches up.
 */

/** A loaded license id ('' = no license), or null when it has not loaded / failed. */
export type EffectiveLicense = string | null;

const NO_LICENSE_KEY = '';

const LICENSE_PHRASE: Record<string, string> = {
  'CC-BY-4.0': 'publish under CC BY 4.0',
  'CC-BY-SA-4.0': 'publish under CC BY-SA 4.0',
  'CC0-1.0': 'publish under CC0 1.0',
  [NO_LICENSE_KEY]: 'publish without a license',
};

const LICENSE_BUTTON: Record<string, string> = {
  'CC-BY-4.0': 'Publish under CC BY 4.0',
  'CC-BY-SA-4.0': 'Publish under CC BY-SA 4.0',
  'CC0-1.0': 'Publish under CC0 1.0',
  [NO_LICENSE_KEY]: 'Publish without a license',
};

/**
 * The lowercase consent phrase for the license, or null when the license has
 * not loaded (so the caller shows a plain notice link and the generic button).
 */
export function licensePhrase(license: EffectiveLicense): string | null {
  if (license == null) return null;
  return LICENSE_PHRASE[license] ?? LICENSE_PHRASE[NO_LICENSE_KEY];
}

/**
 * The capitalized submit-button label naming the license, or null when the
 * license has not loaded (so the caller keeps the generic label).
 */
export function licenseButtonLabel(license: EffectiveLicense): string | null {
  if (license == null) return null;
  return LICENSE_BUTTON[license] ?? LICENSE_BUTTON[NO_LICENSE_KEY];
}
