/**
 * Shell quoting for the commands the settings page prints for a user to paste.
 *
 * The Go CLI renders every command it hands a user with `githooks.ShellQuote`:
 * wrap the value in single quotes and spell an embedded single quote as the
 * four characters `'\''`. A path with a space or a quote is then read literally
 * by a POSIX shell instead of splitting into arguments. This helper keeps the
 * web-rendered commands byte-for-byte the same shape, so the advice the page
 * prints is advice the user can actually run.
 */
export function shellQuote(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`;
}
