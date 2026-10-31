package main

import (
	"bufio"
	"io"
	"strings"
)

// readConsent reads one answer line to a destructive [y/N] prompt and reports
// whether it consents. Only a line that is exactly "y" or "yes" consents,
// compared case-insensitively after trimming surrounding blanks, so "y extra"
// or "yes please" is refused instead of being read as its first word. Case and
// blanks are forgiven because they are typing slips that cannot change the
// answer's meaning; extra words can, so they are not. EOF or a read error is
// a refusal.
func readConsent(r io.Reader) bool {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}
