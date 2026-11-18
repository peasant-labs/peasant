package claimprompt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Choice is one answer to the one-time sign-in ownership prompt.
type Choice string

const (
	// Unclaimed claims only artifacts whose owner pointer is NULL. It is the
	// default for an empty answer and for the prompt's first option.
	Unclaimed Choice = "unclaimed"
	// All reassigns every local session and annotation. It is an explicit
	// reassignment, not history rewriting: earlier claim entries remain.
	All Choice = "all"
	// NotNow dismisses the prompt for one account scope. It is not a claim.
	NotNow Choice = "not_now"
)

// ParseChoice maps raw prompt input to a choice. It is pure: no input is read,
// no state is written, and it accepts the option number or a word for each
// answer. An empty answer selects the default, Unclaimed.
func ParseChoice(input string) (Choice, bool) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "", "1", "u", "unclaimed", "only-unclaimed", "only unclaimed":
		return Unclaimed, true
	case "2", "a", "all", "claim-all", "claim all":
		return All, true
	case "3", "n", "no", "not-now", "not now", "later":
		return NotNow, true
	default:
		return "", false
	}
}

// Prompt asks the three-way question on out and reads the answer from in. It
// repeats on an answer outside the closed set. An EOF before any answer means
// the person dismissed the prompt, so it returns NotNow.
func Prompt(in io.Reader, out io.Writer, username string) (Choice, error) {
	if in == nil {
		return "", fmt.Errorf("ask the local ownership question: no input reader; nothing was claimed")
	}
	if out == nil {
		return "", fmt.Errorf("ask the local ownership question: no output writer; nothing was claimed")
	}
	who := strings.TrimSpace(username)
	if who == "" {
		who = "this peasant online account"
	} else {
		who = "peasant online account " + who
	}
	reader := bufio.NewReader(in)
	for {
		fmt.Fprintf(out,
			"\nClaim your local sessions and annotations for %s?\n"+
				"  1. Claim only unclaimed artifacts (default)\n"+
				"  2. Claim all local sessions and annotations (reassigns existing claims)\n"+
				"  3. Not now\n"+
				"Choice [1/2/3]: ",
			who,
		)
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read the local ownership answer: %w; nothing was claimed", err)
		}
		if errors.Is(err, io.EOF) && strings.TrimSpace(line) == "" {
			return NotNow, nil
		}
		if choice, ok := ParseChoice(line); ok {
			return choice, nil
		}
		fmt.Fprintln(out, "Enter 1, 2, or 3.")
		if errors.Is(err, io.EOF) {
			return NotNow, nil
		}
	}
}
