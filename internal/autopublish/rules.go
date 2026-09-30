// Package autopublish holds the auto-publish rules a developer sets up in
// hooks.yaml, and the one matcher that decides which rules cover a repository.
//
// A rule binds one folder glob or one git remote pattern to Village
// collectives. It is the developer's consent, given once: when a managed hook
// pushes a repository a rule covers, the push publishes that repository's
// sessions redacted, private, and shares each transcript it sent with the
// rule's collectives. A rule never installs a hook by itself. Hooks are
// installed one repository at a time, by an explicit act, and only in a
// repository Peasant has recorded sessions in.
//
// The matcher lives here and nowhere else. The local web shows what it
// decides and never decides it again. Its type is Rule, not Binding:
// githooks.Binding is the path context bound into a generated hook.
package autopublish

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// Rule is one auto-publish rule: the repositories it covers, the hooks that
// publish them, and the collectives each published transcript is shared with.
type Rule struct {
	// ID addresses the rule. It is unique within the file.
	ID string `yaml:"id"`
	// Kind says whether Match is a folder glob or a git remote pattern.
	Kind schema.AutoPublishRuleKind `yaml:"kind"`
	// Match is the pattern, as the developer wrote it.
	Match string `yaml:"match"`
	// Events are the hooks the rule installs. A rule with no event is kept
	// but publishes nothing.
	Events []schema.AutoPublishEvent `yaml:"events"`
	// Collectives are the Village collectives each transcript is shared with.
	Collectives []schema.VillageUUID `yaml:"collectives"`
}

// Validate checks the rule the way the contract does, and that its pattern can
// be read for its kind and its collectives are Village identifiers.
func (r Rule) Validate() error {
	if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.ID) != r.ID || strings.ContainsAny(r.ID, "/?#") {
		return fmt.Errorf("auto-publish rule %q: the identifier is empty, has surrounding spaces, or holds '/', '?', or '#'; a rule is addressed by its identifier in a URL path; use letters, digits, '-', '_', or '.'", r.ID)
	}
	if err := r.Request().Validate(); err != nil {
		return fmt.Errorf("auto-publish rule %q: %w", r.ID, err)
	}
	if err := validateMatch(r.Kind, r.Match); err != nil {
		return fmt.Errorf("auto-publish rule %q: %w", r.ID, err)
	}
	for _, collective := range r.Collectives {
		if !village.IsCollectiveID(collective) {
			return fmt.Errorf("auto-publish rule %q: collective %q is not a Village collective identifier; use the lowercase UUID Village shows for the collective", r.ID, collective)
		}
	}
	return nil
}

// Request is the rule as the contract's request body names it.
func (r Rule) Request() schema.AutoPublishRuleRequest {
	return schema.AutoPublishRuleRequest{Kind: r.Kind, Match: r.Match, Events: nonNil(r.Events), Collectives: nonNil(r.Collectives)}
}

// RuleFromRequest builds the rule id names from a request body.
func RuleFromRequest(id string, request schema.AutoPublishRuleRequest) Rule {
	return Rule{ID: id, Kind: request.Kind, Match: request.Match, Events: request.Events, Collectives: request.Collectives}
}

// Covers reports whether the rule's pattern names the repository. It says
// nothing about whether the rule publishes: a rule with no event covers a
// repository and publishes nothing.
func (r Rule) Covers(repo Repository) bool {
	switch r.Kind {
	case schema.AutoPublishRuleFolder:
		return folderMatches(r.Match, repo.Root)
	case schema.AutoPublishRuleRemote:
		return remoteMatches(r.Match, repo.Remote) || remoteMatches(r.Match, repo.Origin)
	default:
		return false
	}
}

// HookEvents returns the rule's events as the hook events githooks manages.
// The two closed sets name the same hooks; an event githooks does not know
// fails, so a rule never installs a hook it did not name.
func (r Rule) HookEvents() ([]githooks.Event, error) {
	events := make([]githooks.Event, 0, len(r.Events))
	for _, event := range r.Events {
		parsed, err := githooks.ParseEvent(string(event))
		if err != nil {
			return nil, err
		}
		events = append(events, parsed)
	}
	return events, nil
}

// Decision is what the rules decide for one repository.
type Decision struct {
	// Rules are the identifiers of the rules that publish the repository: they
	// cover it and name at least one event. File order.
	Rules []string
	// Collectives are the collectives those rules share with, each once, in
	// the order the rules name them. Overlapping rules add their collectives
	// together: each is a binding the developer set up.
	Collectives []schema.VillageUUID
	// Events are the hook events those rules name, each once.
	Events []schema.AutoPublishEvent
	// Paused are the identifiers of the rules that cover the repository but
	// name no event. A paused rule publishes nothing, and it still says the
	// developer bound the repository, so a push must not publish it another
	// way.
	Paused []string
}

// Covered reports whether any rule covers the repository, publishing or
// paused.
func (d Decision) Covered() bool { return len(d.Rules) > 0 || len(d.Paused) > 0 }

// Decide runs the one matcher: which rules cover the repository, and what they
// publish it to.
func Decide(rules []Rule, repo Repository) Decision {
	var decision Decision
	for _, rule := range rules {
		if !rule.Covers(repo) {
			continue
		}
		if len(rule.Events) == 0 {
			decision.Paused = append(decision.Paused, rule.ID)
			continue
		}
		decision.Rules = append(decision.Rules, rule.ID)
		for _, collective := range rule.Collectives {
			if !slices.Contains(decision.Collectives, collective) {
				decision.Collectives = append(decision.Collectives, collective)
			}
		}
		for _, event := range rule.Events {
			if !slices.Contains(decision.Events, event) {
				decision.Events = append(decision.Events, event)
			}
		}
	}
	return decision
}

// --- folder globs ---

// folderSegments reads a folder glob into its path segments: a leading "~"
// names the home directory, and the result must be absolute.
func folderSegments(pattern string) ([]string, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "~" || strings.HasPrefix(pattern, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("the folder glob %q starts with '~', but the home directory is unknown: %w; write the absolute path", pattern, err)
		}
		pattern = filepath.Join(home, strings.TrimPrefix(pattern, "~"))
	}
	if !filepath.IsAbs(pattern) {
		return nil, fmt.Errorf("the folder glob %q is not an absolute path; write it from the root, or from '~' for your home directory", pattern)
	}
	return splitPath(filepath.ToSlash(filepath.Clean(pattern))), nil
}

func splitPath(p string) []string {
	var segments []string
	for _, segment := range strings.Split(p, "/") {
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	return segments
}

// folderMatches reports whether the glob names the repository root. Each
// segment matches one path segment with filepath.Match; "**" matches any
// number of segments, none included. The glob names the root itself: a
// repository nested in a matched one is a separate repository and is not
// covered unless the glob names it too.
func folderMatches(pattern, root string) bool {
	if strings.TrimSpace(root) == "" {
		return false
	}
	segments, err := folderSegments(pattern)
	if err != nil {
		return false
	}
	path := splitPath(filepath.ToSlash(filepath.Clean(root)))
	if matchSegments(segments, path) {
		return true
	}
	// Git reports a root with its symlinks resolved, so /tmp/x arrives as
	// /private/tmp/x. The glob's literal prefix is resolved the same way.
	resolved := resolveLiteralPrefix(segments)
	return !slices.Equal(resolved, segments) && matchSegments(resolved, path)
}

// resolveLiteralPrefix resolves the symlinks in the leading segments that hold
// no glob character, and keeps the rest of the glob as it is.
func resolveLiteralPrefix(segments []string) []string {
	literal := 0
	for literal < len(segments) && !strings.ContainsAny(segments[literal], globMeta) {
		literal++
	}
	for ; literal > 0; literal-- {
		prefix := "/" + strings.Join(segments[:literal], "/")
		if real, err := filepath.EvalSymlinks(filepath.FromSlash(prefix)); err == nil {
			return append(splitPath(filepath.ToSlash(real)), segments[literal:]...)
		}
	}
	return segments
}

// globMeta are the characters filepath.Match reads as a pattern.
const globMeta = `*?[\`

// FolderMatch is the folder glob that names exactly the directory root: each
// glob character in it is escaped, so a folder named "a*b" or "[old]" is
// covered, and a sibling is not.
func FolderMatch(root string) string {
	segments := splitPath(filepath.ToSlash(filepath.Clean(root)))
	for i, segment := range segments {
		var b strings.Builder
		for _, r := range segment {
			if strings.ContainsRune(globMeta, r) {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		segments[i] = b.String()
	}
	return "/" + strings.Join(segments, "/")
}

func matchSegments(pattern, path []string) bool {
	if len(pattern) == 0 {
		return len(path) == 0
	}
	if pattern[0] == "**" {
		for skip := 0; skip <= len(path); skip++ {
			if matchSegments(pattern[1:], path[skip:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 {
		return false
	}
	ok, err := filepath.Match(pattern[0], path[0])
	return err == nil && ok && matchSegments(pattern[1:], path[1:])
}

// --- git remote patterns ---

// remotePatternLabel reads a remote pattern into the schema.RemoteLabel form
// "host:path", lowercased. The pattern is any remote git accepts, or its bare
// "host/owner/repo" form, and its path may hold glob characters.
func remotePatternLabel(pattern string) (string, error) {
	pattern = strings.TrimSpace(pattern)
	// "host:owner/repo" is git's SCP form without a user. RemoteLabel reads
	// the SCP form only with one, so name the user git would use. A colon
	// followed by digits and a slash is a port of the bare form instead.
	if !strings.Contains(pattern, "://") && !strings.Contains(pattern, "@") {
		if host, rest, found := strings.Cut(pattern, ":"); found && host != "" && !strings.Contains(host, "/") && !isPort(rest) {
			pattern = "git@" + pattern
		}
	}
	label, ok := schema.RemoteLabel(pattern)
	if !ok {
		return "", fmt.Errorf("the remote pattern %q names no host and repository path; write it like github.com/owner/repo, github.com/owner/*, or a remote URL", pattern)
	}
	return strings.ToLower(label), nil
}

// isPort reports whether rest starts with a port number and a slash.
func isPort(rest string) bool {
	port, _, found := strings.Cut(rest, "/")
	if !found || port == "" {
		return false
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// remoteMatches reports whether the pattern names the repository's remote.
// Both sides are compared as schema.RemoteLabel, without case, as GitHub
// resolves names; "*" in the pattern matches within one path segment.
func remoteMatches(pattern, remote string) bool {
	label, ok := schema.RemoteLabel(remote)
	if !ok {
		return false
	}
	want, err := remotePatternLabel(pattern)
	if err != nil {
		return false
	}
	matched, err := path.Match(want, strings.ToLower(label))
	return err == nil && matched
}

// validateMatch checks that the pattern can be read for its kind.
func validateMatch(kind schema.AutoPublishRuleKind, match string) error {
	switch kind {
	case schema.AutoPublishRuleFolder:
		segments, err := folderSegments(match)
		if err != nil {
			return err
		}
		for _, segment := range segments {
			if _, err := filepath.Match(segment, ""); errors.Is(err, filepath.ErrBadPattern) {
				return fmt.Errorf("the folder glob %q has a malformed segment %q; close every '[' and escape a literal '\\'", match, segment)
			}
		}
		return nil
	case schema.AutoPublishRuleRemote:
		label, err := remotePatternLabel(match)
		if err != nil {
			return err
		}
		if _, err := path.Match(label, ""); errors.Is(err, path.ErrBadPattern) {
			return fmt.Errorf("the remote pattern %q is malformed; close every '[' and escape a literal '\\'", match)
		}
		if _, repoPath, _ := strings.Cut(label, ":"); !strings.Contains(repoPath, "/") {
			return fmt.Errorf("the remote pattern %q names an owner but no repository, so it matches no remote; write %s/* for every repository of the owner", match, strings.TrimSuffix(match, "/"))
		}
		return nil
	default:
		return fmt.Errorf("kind %q is not folder or remote", kind)
	}
}

// RemoteMatch is the remote pattern a rule for exactly this remote uses: its
// bare "host/owner/repo" form, or "" when the remote has none.
func RemoteMatch(remote string) string {
	label, ok := schema.RemoteLabel(remote)
	if !ok {
		return ""
	}
	host, repoPath, _ := strings.Cut(label, ":")
	return host + "/" + repoPath
}

func nonNil[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}
