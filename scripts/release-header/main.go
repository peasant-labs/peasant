// Command release-header extracts the release's section from CHANGELOG.md.
// The release workflow supplies it as GoReleaser's header, leaving the
// generated commit list and the keep-existing rerun policy intact.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

type line struct {
	start int
	text  string
}

// section returns the original section bytes without the blank lines that
// separate it from the next section. Tags have already passed release-guard;
// this boundary only removes the leading v, not any prerelease suffix.
func section(changelog []byte, tag string) ([]byte, error) {
	if !strings.HasPrefix(tag, "v") || len(tag) == 1 || strings.ContainsAny(tag, "\r\n") {
		return nil, fmt.Errorf("tag %q must start with v and contain a version; use the tag checked by release-guard", tag)
	}
	if bytes.IndexByte(changelog, 0) >= 0 {
		return nil, fmt.Errorf("CHANGELOG.md contains a NUL byte; remove it before generating release notes")
	}
	want := "[" + tag[1:] + "]"
	var lines []line
	for start := 0; start < len(changelog); {
		end := bytes.IndexByte(changelog[start:], '\n')
		if end < 0 {
			end = len(changelog)
		} else {
			end += start + 1
		}
		lines = append(lines, line{start: start, text: strings.TrimSuffix(strings.TrimSuffix(string(changelog[start:end]), "\n"), "\r")})
		start = end
	}

	start, end, matches := -1, len(changelog), 0
	var fence byte
	fenceLength := 0
	inComment := false
	// openedLine and openedAt are the line and byte offset where the fence or comment that is
	// still open began, so a construct left open can be named.
	openedLine, openedAt := 0, 0
	for index, current := range lines {
		text := current.text
		trimmed := strings.TrimLeft(text, " ")
		if inComment {
			if strings.Contains(trimmed, "-->") {
				inComment = false
			}
			continue
		}
		if fence == 0 && strings.HasPrefix(trimmed, "<!--") {
			inComment = !strings.Contains(trimmed, "-->")
			if inComment {
				openedLine, openedAt = index+1, current.start
			}
			continue
		}
		// Markdown permits up to three spaces before a fenced code block.
		isFence := len(text)-len(trimmed) <= 3 && len(trimmed) >= 3 &&
			(trimmed[0] == '`' || trimmed[0] == '~')
		if isFence {
			marker := trimmed[0]
			length := 0
			for length < len(trimmed) && trimmed[length] == marker {
				length++
			}
			if length >= 3 {
				if fence == 0 {
					fence, fenceLength = marker, length
					openedLine, openedAt = index+1, current.start
				} else if fence == marker && length >= fenceLength && strings.TrimSpace(trimmed[length:]) == "" {
					fence = 0
				}
				continue
			}
		}
		if fence != 0 || !strings.HasPrefix(text, "## ") {
			continue
		}
		heading := strings.TrimSpace(strings.TrimPrefix(text, "## "))
		if start >= 0 && end == len(changelog) {
			end = current.start
		}
		if heading == want || strings.HasPrefix(heading, want+" ") {
			matches++
			if start < 0 {
				start = current.start
			}
		}
	}
	// A fence or comment left open swallows every heading after it, so the
	// section would run to the end of the file and publish every older release
	// as this one's notes. An open construct that began after the section's end
	// cannot change the section, so it is not this release's problem.
	if (fence != 0 || inComment) && openedAt < end {
		kind := "code fence"
		closer := "a matching closing fence line"
		if inComment {
			kind, closer = "HTML comment", "-->"
		}
		return nil, fmt.Errorf("CHANGELOG.md has an unclosed %s opened on line %d, so the section for tag %q cannot be bounded and the release notes would include every older section; close it with %s before tagging", kind, openedLine, tag, closer)
	}
	if matches == 0 {
		return nil, fmt.Errorf("CHANGELOG.md has no section for tag %q; add a heading like ## %s to the release commit before tagging", tag, want)
	}
	if matches > 1 {
		return nil, fmt.Errorf("CHANGELOG.md has multiple sections for tag %q; keep exactly one ## %s heading before tagging", tag, want)
	}

	// Keep a final section separate from the document-wide link definitions at
	// the end of a Keep a Changelog document.
	if end == len(changelog) {
		footer := end
		definitions := false
		for i := len(lines) - 1; i >= 0 && lines[i].start > start; i-- {
			text := strings.TrimSpace(lines[i].text)
			if text == "" {
				footer = lines[i].start
				continue
			}
			if strings.HasPrefix(text, "[") && strings.Contains(text, "]: ") {
				definitions = true
				footer = lines[i].start
				continue
			}
			break
		}
		if definitions {
			end = footer
		}
	}
	return bytes.TrimRight(changelog[start:end], "\r\n"), nil
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("release-header", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("changelog", "CHANGELOG.md", "path to the changelog in the release commit")
	tag := flags.String("tag", "", "release tag, including v and any rc suffix")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *tag == "" {
		return fmt.Errorf("provide -tag vX.Y.Z[-rcN] and optionally -changelog PATH")
	}
	data, err := os.ReadFile(*path)
	if err != nil {
		return fmt.Errorf("read changelog %q for %s: %w", *path, *tag, err)
	}
	header, err := section(data, *tag)
	if err != nil {
		return err
	}
	if _, err := stdout.Write(header); err != nil {
		return fmt.Errorf("write release header for %s: %w", *tag, err)
	}
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "release-header: %v\n", err)
		os.Exit(1)
	}
}
