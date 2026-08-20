package kickstart

import tea "charm.land/bubbletea/v2"

// LoginResultForTest builds the internal login-result message so external tests
// can feed a specific attempt result (including a stale one, by epoch) without
// driving the injected runner. It exists only in test builds.
func LoginResultForTest(username string, err error, epoch uint64) tea.Msg {
	return loginDoneMsg{username: username, err: err, epoch: epoch}
}

// LoginURLForTest builds the internal login-URL report message so external
// tests can feed a specific attempt's reported URL (including a stale one, by
// epoch) without driving the injected runner's onURL callback. It exists only
// in test builds.
func LoginURLForTest(url string, epoch uint64) tea.Msg {
	return loginURLMsg{url: url, epoch: epoch}
}

// VillageContextBulletsForTest exposes the connect-prompt facts so external
// tests can prove none of them carries an embedded hard-wrap newline (the
// prompt copy must be a list of short sentences, not a wrapped prose block).
// It exists only in test builds.
func VillageContextBulletsForTest() []string {
	return append([]string(nil), villageContextBullets...)
}

// VisibilityContextBulletsForTest is VillageContextBulletsForTest's twin for
// the visibility-login prompt's facts. It exists only in test builds.
func VisibilityContextBulletsForTest() []string {
	return append([]string(nil), visibilityContextBullets...)
}
