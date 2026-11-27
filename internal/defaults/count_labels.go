package defaults

// SessionWord returns the singular or plural presentation for a session count.
func SessionWord(count int) string {
	if count == 1 {
		return "session"
	}
	return "sessions"
}
