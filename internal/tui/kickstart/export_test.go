package kickstart

import tea "charm.land/bubbletea/v2"

// LoginResultForTest builds the internal login-result message so external tests
// can feed a specific attempt result (including a stale one, by epoch) without
// driving the injected runner. It exists only in test builds.
func LoginResultForTest(username string, err error, epoch uint64) tea.Msg {
	return loginDoneMsg{username: username, err: err, epoch: epoch}
}
