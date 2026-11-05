// Package sessionreview finds what redaction leaves behind in a session's
// entries. It only reports: it never produces replacement text, and a finding
// never carries the matched value, only a masked preview.
//
// A pattern that should be redacted belongs in github.com/peasant-labs/redact.
// When such a rule ships, the matching detector here is deleted.
package sessionreview

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	regexp "github.com/wasilibs/go-re2"
)

// FindingKind names what a detector found. It is a closed set.
type FindingKind string

const (
	// KindCredentialAssignment is a secret-named value that no rule redacted.
	KindCredentialAssignment FindingKind = "credential_assignment"
	// KindCredentialFileOutput is a tool result that printed credentials.
	KindCredentialFileOutput FindingKind = "credential_file_output"
	// KindForeignEmail is an address that survived redaction and is not the
	// owner's.
	KindForeignEmail FindingKind = "foreign_email"
	// KindShareableDocumentLink is a URL that grants access to anyone holding
	// it.
	KindShareableDocumentLink FindingKind = "shareable_document_link"
	// KindRedactionResidue is an advisory hit from the redact module's own
	// residue rules.
	KindRedactionResidue FindingKind = "redaction_residue"
)

// Advisory reports whether findings of kind k are advisory. The redact
// module's residue rules are broad on purpose and hit on commit ids, digests,
// module paths and long repository paths, so their hits are reported but never
// make a session's status findings. Every other kind comes from a detector of
// this package and is blocking.
func (k FindingKind) Advisory() bool {
	return k == KindRedactionResidue
}

// AllFindingKinds is the closed set of finding kinds.
var AllFindingKinds = []FindingKind{
	KindCredentialAssignment,
	KindCredentialFileOutput,
	KindForeignEmail,
	KindShareableDocumentLink,
	KindRedactionResidue,
}

// NewFindingKind validates raw against the closed set.
func NewFindingKind(raw string) (FindingKind, error) {
	kind := FindingKind(raw)
	if !slices.Contains(AllFindingKinds, kind) {
		return "", fmt.Errorf("unknown finding kind %q", raw)
	}
	return kind, nil
}

// Rule ids of the detectors. A residue finding carries the redact rule id
// instead, and a link finding carries its host table entry's Rule.
const (
	ruleCredentialNameValue = "credential_name_value"
	ruleCredentialFileName  = "credential_file_name"
	ruleCredentialFileWrite = "credential_file_write"
	ruleSecretEnvListing    = "secret_env_listing"
	ruleInlinePassword      = "inline_password_flag"
	ruleForeignEmail        = "foreign_email"
)

// redactResidueEmailRule is left out of the passthrough: an address that
// survives redaction is reported by foreign_email, which knows the owner set.
const redactResidueEmailRule = "residue_email"

// Finding is one thing a detector found. Preview is masked and at most
// redact.PreviewMaxLength bytes. Advisory is Kind.Advisory(): an advisory
// finding is reported and does not block.
type Finding struct {
	Kind       FindingKind
	EntryIndex int
	Role       schema.Role
	Rule       string
	Preview    string
	Advisory   bool
}

// Blocking reports whether any of findings is not advisory.
func Blocking(findings []Finding) bool {
	return slices.ContainsFunc(findings, func(f Finding) bool { return !f.Advisory })
}

// Options configures a review.
type Options struct {
	// OwnerEmails are the addresses that count as the owner's.
	OwnerEmails []string
}

// Detector thresholds.
const (
	minAssignmentValueLength = 20
	minAssignmentEntropy     = 3.5
	// A value of a password-named name may hold symbols and no digit, so it is
	// held to a shorter length and a lower entropy.
	minPasswordValueLength   = 8
	minPasswordEntropy       = 3.0
	minEnvListingLines       = 2
	minEnvListingValueLength = 12
	maxPreviewNameLength     = 20
	maskedName               = "<name>"
	masked                   = "****"
)

// secretNameParts is the closed list of words that make a name secret-shaped.
var secretNameParts = []string{"key", "secret", "token", "password", "credential"}

// PasswordNameSuffixes is the closed list of name endings, compared in lower
// case, that name one credential value: DB_PASSWORD, pgpassword,
// client_secret, GITHUB_TOKEN. The value of such a name is caught with symbols
// in it or with no digit. A name that only contains the word, such as
// PASSWORD_MIN_LENGTH or TOKEN_TTL, does not end with it.
var PasswordNameSuffixes = []string{"password", "passwd", "passphrase", "secret", "token"}

// InlinePasswordCommands is the closed list of commands whose -p<value> flag
// is a password written on the command line. A -p followed by a space asks
// for the password instead and is not a finding.
var InlinePasswordCommands = []string{"mysql", "mysqldump", "mysqladmin", "mariadb", "mariadb-dump"}

// CredentialFileNames is the closed list of file names whose content is a
// credential. Names are compared in lower case.
var CredentialFileNames = []string{
	".credentials.json",
	".env",
	".netrc",
	".npmrc",
	".pgpass",
	".pypirc",
	"application_default_credentials.json",
	"auth.json",
	"credentials",
	"credentials.json",
	"id_ed25519",
	"id_rsa",
	"secrets.json",
	"secrets.yaml",
	"secrets.yml",
	"service-account.json",
	"service_account.json",
	"token.json",
}

// CredentialFileNamePrefixes is the closed list of file name beginnings whose
// file is a credential, such as .env.local and .env.production. A name that
// also ends with one of CredentialFileTemplateSuffixes is a template that
// holds no values, and is not one.
var CredentialFileNamePrefixes = []string{".env."}

// CredentialFileTemplateSuffixes is the closed list of endings that make a
// prefixed credential file name a template.
var CredentialFileTemplateSuffixes = []string{".dist", ".example", ".sample", ".template"}

// CredentialFilePaths is the closed list of path endings whose file is a
// credential although its base name alone is common.
var CredentialFilePaths = []string{".kube/config"}

// writePathKeys and writeContentKeys are the tool input keys of a call that
// writes a file: its path, and the content it writes.
var (
	writePathKeys    = []string{"file_path", "filePath", "path", "notebook_path"}
	writeContentKeys = []string{"content", "new_string", "newString", "new_source", "edits"}
)

// ShareableHost is one document host whose URLs of a path shape grant access
// to anyone holding the link. The first path segment of every shape is a fixed
// word, so the masked preview never shows a document id.
type ShareableHost struct {
	Rule string
	// Host is the exact host, or a suffix when it starts with a dot.
	Host string
	// Path matches the URL path and query.
	Path *regexp.Regexp
}

// ShareableHosts is the closed list of link-shareable document hosts.
var ShareableHosts = []ShareableHost{
	{Rule: "share_google_docs", Host: "docs.google.com", Path: regexp.MustCompile(`^/(document|spreadsheets|presentation|forms)/d/[^/?]+`)},
	{Rule: "share_google_drive", Host: "drive.google.com", Path: regexp.MustCompile(`^/(file/d/[^/?]+|drive/folders/[^/?]+|open\?(.*&)?id=)`)},
	{Rule: "share_dropbox", Host: "dropbox.com", Path: regexp.MustCompile(`^/(s|sh|scl/fi|scl/fo)/[^/?]+`)},
	{Rule: "share_dropbox", Host: "www.dropbox.com", Path: regexp.MustCompile(`^/(s|sh|scl/fi|scl/fo)/[^/?]+`)},
	{Rule: "share_onedrive", Host: "1drv.ms", Path: regexp.MustCompile(`^/[a-z](/[a-z])?/[^/?]+`)},
	{Rule: "share_sharepoint", Host: ".sharepoint.com", Path: regexp.MustCompile(`^/:[a-z]:/`)},
	{Rule: "share_box", Host: "app.box.com", Path: regexp.MustCompile(`^/s/[^/?]+`)},
}

var (
	assignmentPattern = regexp.MustCompile(`["']?([A-Za-z][A-Za-z0-9_.-]{0,63})["']?\s*[:=]\s*["']?([A-Za-z0-9+/=_.~-]{20,})`)
	envLinePattern    = regexp.MustCompile(`(?m)^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=(\S+)\s*$`)
	// The address pattern is wider than the redact rule on purpose: it holds
	// any letter, so it sees the addresses that rule cannot match.
	emailPattern    = regexp.MustCompile(`[\p{L}\p{N}._%+\-]+@[\p{L}\p{N}.\-]+\.\p{L}{2,}`)
	urlPattern      = regexp.MustCompile(`https?://[^\s"'<>)\]]+`)
	pathTokenSplits = regexp.MustCompile(`[\s"'=:,;()]+`)
	// passwordAssignmentPattern takes a quoted value whole, or an unquoted one
	// up to a space, quote, comma or semicolon, so symbols stay in it.
	passwordAssignmentPattern = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])["']?([A-Za-z_][A-Za-z0-9_.-]{0,63})["']?\s*[:=]\s*(?:"([^"\s]{8,128})"|'([^'\s]{8,128})'|([^\s"',;]{8,128}))`)
	inlinePasswordPattern     = regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])(` + strings.Join(InlinePasswordCommands, "|") + `)(?:[ \t][^\n|;&]*?)?[ \t]-p(\S+)`)
	// lineNumberPrefix is the line number a Read tool prints ahead of each
	// line, as cat -n does ("     1<TAB>") or as "1→".
	lineNumberPrefix = regexp.MustCompile(`(?m)^[ \t]*[0-9]+(?:\t|→)`)
	dottedIdentifier = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)+$`)
	bareIdentifier   = regexp.MustCompile(`^[A-Za-z_]+$`)
)

// span is a byte range of a redacted text that a detector has reported.
type span struct{ start, end int }

// reviewer holds one Review call's state.
type reviewer struct {
	redactor redact.Redactor
	residue  redact.ResidueDetector
	owners   map[string]bool
	seen     map[Finding]bool
	findings []Finding
}

// Review scans entries after r has redacted them. It never returns matched
// text, and it keeps the redacted text in memory only.
func Review(entries []schema.SessionEntry, r redact.Redactor, opts Options) []Finding {
	rv := &reviewer{
		redactor: r,
		residue:  redact.NewResidueDetector(),
		owners:   make(map[string]bool, len(opts.OwnerEmails)),
		seen:     map[Finding]bool{},
	}
	for _, email := range opts.OwnerEmails {
		rv.owners[strings.ToLower(strings.TrimSpace(email))] = true
	}
	calls := rv.toolCalls(entries)
	for i := range entries {
		rv.reviewEntry(&entries[i], calls)
	}
	return rv.findings
}

// reviewText is the text the detectors read: a JSON document decoded to its
// strings, so an escaped newline is a line again, with the line numbers a Read
// tool prints removed, so a line pattern sees the line itself.
func reviewText(text string) string {
	if decoded, ok := decodeJSONText(text); ok {
		text = decoded
	}
	return lineNumberPrefix.ReplaceAllString(text, "")
}

// decodeJSONText renders a JSON document as text: an object member as
// "key: value", in key order, and an array element on its own line. A string
// that holds a newline starts on its own line, so its lines are lines.
func decodeJSONText(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || !strings.ContainsRune(`[{"`, rune(trimmed[0])) {
		return "", false
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil || decoder.More() {
		return "", false
	}
	var out strings.Builder
	writeJSONText(&out, "", value)
	return out.String(), true
}

func writeJSONText(out *strings.Builder, key string, value any) {
	prefix := ""
	if key != "" {
		prefix = key + ": "
	}
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			writeJSONText(out, k, v[k])
		}
	case []any:
		for _, element := range v {
			writeJSONText(out, key, element)
		}
	case string:
		if strings.Contains(v, "\n") {
			if key != "" {
				out.WriteString(key + ":\n")
			}
			out.WriteString(v + "\n")
			return
		}
		out.WriteString(prefix + v + "\n")
	case nil:
		out.WriteString(prefix + "null\n")
	default:
		out.WriteString(prefix + fmt.Sprint(v) + "\n")
	}
}

// toolCalls maps each tool call id to the redacted input of the call, so a
// result can be paired with the call that produced it.
func (rv *reviewer) toolCalls(entries []schema.SessionEntry) map[string]schema.SessionEntry {
	calls := map[string]schema.SessionEntry{}
	for _, entry := range entries {
		if entry.ToolCallID == nil || entry.ToolInput == nil {
			continue
		}
		redacted := rv.redactor.RedactText(*entry.ToolInput)
		entry.ToolInput = &redacted
		calls[*entry.ToolCallID] = entry
	}
	return calls
}

func (rv *reviewer) reviewEntry(entry *schema.SessionEntry, calls map[string]schema.SessionEntry) {
	if entry.ContentPreview != nil {
		rv.scan(entry, reviewText(rv.redactor.RedactText(*entry.ContentPreview)), entry.Role == schema.RoleUser, false)
	}
	if entry.ToolInput != nil {
		input := rv.redactor.RedactText(*entry.ToolInput)
		covered := false
		if name, ok := credentialFileWrite(input); ok {
			rv.add(newFinding(entry, KindCredentialFileOutput, ruleCredentialFileWrite, name+": written content="+masked))
			covered = true
		}
		rv.scan(entry, reviewText(input), false, covered)
	}
	if entry.ToolOutput == nil {
		return
	}
	output := reviewText(rv.redactor.RedactText(*entry.ToolOutput))
	covered := false
	if finding, ok := credentialFileFinding(entry, output, calls); ok {
		rv.add(finding)
		covered = true
	}
	rv.scan(entry, output, true, covered)
}

// scan runs the text detectors on one redacted text. covered says a finding
// already accounts for the whole text, so its secret-shaped lines are not
// reported a second time.
func (rv *reviewer) scan(entry *schema.SessionEntry, text string, scanEmails, covered bool) {
	if text == "" {
		return
	}
	var spans []span
	if !covered {
		spans = append(spans, rv.assignments(entry, text)...)
	}
	if scanEmails {
		rv.emails(entry, text)
	}
	spans = append(spans, rv.links(entry, text)...)
	if !covered {
		rv.residuePassthrough(entry, text, spans)
	}
}

func (rv *reviewer) add(finding Finding) {
	finding.Preview = clampPreview(finding.Preview)
	if rv.seen[finding] {
		return
	}
	rv.seen[finding] = true
	rv.findings = append(rv.findings, finding)
}

func newFinding(entry *schema.SessionEntry, kind FindingKind, rule, preview string) Finding {
	return Finding{Kind: kind, EntryIndex: entry.EntryIndex, Role: entry.Role, Rule: rule, Preview: preview, Advisory: kind.Advisory()}
}

func (rv *reviewer) assignments(entry *schema.SessionEntry, text string) []span {
	var spans []span
	for _, match := range assignmentPattern.FindAllStringSubmatchIndex(text, -1) {
		name, value := text[match[2]:match[3]], text[match[4]:match[5]]
		if !secretNamed(name) || !secretShapedValue(value) {
			continue
		}
		preview := previewName(name) + "=" + masked + " " + lengthClass(len(value))
		rv.add(newFinding(entry, KindCredentialAssignment, ruleCredentialNameValue, preview))
		spans = append(spans, span{match[0], match[1]})
	}
	for _, match := range passwordAssignmentPattern.FindAllStringSubmatchIndex(text, -1) {
		name := text[match[2]:match[3]]
		var value string
		for group := 4; group < len(match); group += 2 {
			if match[group] >= 0 {
				value = text[match[group]:match[group+1]]
			}
		}
		if !passwordNamed(name) || !passwordShapedValue(value) {
			continue
		}
		preview := previewName(name) + "=" + masked + " " + lengthClass(len(value))
		rv.add(newFinding(entry, KindCredentialAssignment, ruleCredentialNameValue, preview))
		spans = append(spans, span{match[0], match[1]})
	}
	for _, match := range inlinePasswordPattern.FindAllStringSubmatchIndex(text, -1) {
		command := text[match[2]:match[3]]
		value := strings.Trim(text[match[4]:match[5]], `"'`)
		if value == "" || strings.HasPrefix(value, "$") || strings.ContainsAny(value, "<>") {
			continue
		}
		preview := command + " -p" + masked + " " + lengthClass(len(value))
		rv.add(newFinding(entry, KindCredentialAssignment, ruleInlinePassword, preview))
		spans = append(spans, span{match[0], match[1]})
	}
	return spans
}

func passwordNamed(name string) bool {
	lower := strings.ToLower(name)
	return slices.ContainsFunc(PasswordNameSuffixes, func(suffix string) bool { return strings.HasSuffix(lower, suffix) })
}

// passwordShapedValue is a value a password-named name may hold: long enough,
// not low in entropy, and not a placeholder, a reference, a path, a URL or a
// code expression. It may hold symbols and need not hold a digit, but a value
// of letters alone is an identifier, such as hashedPassword.
func passwordShapedValue(value string) bool {
	if len(value) < minPasswordValueLength || shannonEntropy(value) < minPasswordEntropy {
		return false
	}
	if strings.ContainsAny(value, "<>(") || strings.Contains(value, "://") {
		return false
	}
	for _, prefix := range []string{"$", "%", "{", "[", "/", "~/", "./", "../"} {
		if strings.HasPrefix(value, prefix) {
			return false
		}
	}
	if dottedIdentifier.MatchString(value) || bareIdentifier.MatchString(value) {
		return false
	}
	return strings.IndexFunc(value, unicode.IsLetter) >= 0
}

func secretNamed(name string) bool {
	lower := strings.ToLower(name)
	return slices.ContainsFunc(secretNameParts, func(part string) bool { return strings.Contains(lower, part) })
}

// secretShapedValue is long, mixes letters and digits, and is high in entropy.
func secretShapedValue(value string) bool {
	if len(value) < minAssignmentValueLength || shannonEntropy(value) < minAssignmentEntropy {
		return false
	}
	return strings.ContainsAny(value, "0123456789") && strings.IndexFunc(value, unicode.IsLetter) >= 0
}

func shannonEntropy(value string) float64 {
	counts := map[rune]int{}
	total := 0
	for _, r := range value {
		counts[r]++
		total++
	}
	entropy := 0.0
	for _, count := range counts {
		p := float64(count) / float64(total)
		entropy -= p * math.Log2(p)
	}
	return entropy
}

// previewName is the name of an assignment as a preview may show it. A long or
// random-looking name may itself be a value, so it is replaced.
func previewName(name string) string {
	if len(name) > maxPreviewNameLength || shannonEntropy(name) >= minAssignmentEntropy+0.5 {
		return maskedName
	}
	return name
}

func lengthClass(length int) string {
	switch {
	case length < 32:
		return "(len<32)"
	case length < 64:
		return "(len 32-63)"
	default:
		return "(len 64+)"
	}
}

// credentialFileFinding reports a tool result that printed credentials: its
// call read a credential-shaped file, or the output lists secret-named values.
func credentialFileFinding(entry *schema.SessionEntry, output string, calls map[string]schema.SessionEntry) (Finding, bool) {
	if entry.IsError || strings.TrimSpace(output) == "" {
		return Finding{}, false
	}
	if entry.ToolCallID != nil {
		if call, ok := calls[*entry.ToolCallID]; ok {
			if name, found := credentialFileRead(call); found {
				return newFinding(entry, KindCredentialFileOutput, ruleCredentialFileName, name+": content="+masked), true
			}
		}
	}
	if lines, ok := secretEnvListing(output); ok {
		return newFinding(entry, KindCredentialFileOutput, ruleSecretEnvListing, fmt.Sprintf("%d secret-named lines=%s", lines, masked)), true
	}
	return Finding{}, false
}

// credentialFileRead returns the credential file name a tool call read. A call
// that writes a file does not print it, so it never counts here: its input is
// reported by credentialFileWrite instead.
func credentialFileRead(call schema.SessionEntry) (string, bool) {
	if call.ToolInput == nil {
		return "", false
	}
	if call.ToolKind != nil && (*call.ToolKind == schema.ToolCallKindEdit || *call.ToolKind == schema.ToolCallKindDelete) {
		return "", false
	}
	if _, writes := writeInput(*call.ToolInput); writes {
		return "", false
	}
	for _, token := range pathTokenSplits.Split(*call.ToolInput, -1) {
		if name, ok := credentialFileName(token); ok {
			return name, true
		}
	}
	return "", false
}

// credentialFileName returns how a preview names a credential file path: its
// base name, or the path ending that makes it one.
func credentialFileName(raw string) (string, bool) {
	cleaned := strings.ToLower(strings.ReplaceAll(raw, `\`, "/"))
	base := path.Base(cleaned)
	if slices.Contains(CredentialFileNames, base) {
		return base, true
	}
	if slices.ContainsFunc(CredentialFileNamePrefixes, func(prefix string) bool { return strings.HasPrefix(base, prefix) && len(base) > len(prefix) }) &&
		!slices.ContainsFunc(CredentialFileTemplateSuffixes, func(suffix string) bool { return strings.HasSuffix(base, suffix) }) {
		return base, true
	}
	for _, ending := range CredentialFilePaths {
		if cleaned == ending || strings.HasSuffix(cleaned, "/"+ending) {
			return ending, true
		}
	}
	return "", false
}

// writeInput returns the path a tool call's JSON input writes content to. A
// call writes when its input names a path and carries non-empty content, as a
// Write or an Edit call does.
func writeInput(input string) (string, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(input), &fields); err != nil {
		return "", false
	}
	content := slices.ContainsFunc(writeContentKeys, func(key string) bool {
		raw, ok := fields[key]
		return ok && hasText(raw)
	})
	if !content {
		return "", false
	}
	for _, key := range writePathKeys {
		var target string
		if raw, ok := fields[key]; ok && json.Unmarshal(raw, &target) == nil && target != "" {
			return target, true
		}
	}
	return "", false
}

// hasText reports whether a JSON value holds a non-blank string anywhere.
func hasText(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch v := v.(type) {
		case string:
			return strings.TrimSpace(v) != ""
		case []any:
			return slices.ContainsFunc(v, walk)
		case map[string]any:
			for _, element := range v {
				if walk(element) {
					return true
				}
			}
		}
		return false
	}
	return walk(value)
}

// credentialFileWrite returns the credential file name a tool call's input
// writes content to.
func credentialFileWrite(input string) (string, bool) {
	target, ok := writeInput(input)
	if !ok {
		return "", false
	}
	return credentialFileName(target)
}

// secretEnvListing counts the secret-named NAME=value lines of an output. A
// redacted value still counts as a line, but the listing is reported only when
// a value survived redaction: otherwise nothing is left to find.
func secretEnvListing(output string) (int, bool) {
	lines, survived := 0, false
	for _, match := range envLinePattern.FindAllStringSubmatch(output, -1) {
		if !secretNamed(match[1]) {
			continue
		}
		lines++
		if len(match[2]) >= minEnvListingValueLength && !strings.ContainsAny(match[2], "<>") {
			survived = true
		}
	}
	return lines, lines >= minEnvListingLines && survived
}

func (rv *reviewer) emails(entry *schema.SessionEntry, text string) {
	for _, address := range emailPattern.FindAllString(text, -1) {
		lower := strings.ToLower(address)
		if rv.owners[lower] || strings.HasPrefix(lower, "git@") {
			continue
		}
		rv.add(newFinding(entry, KindForeignEmail, ruleForeignEmail, maskEmail(address)))
	}
}

func maskEmail(address string) string {
	local, domain, _ := strings.Cut(address, "@")
	first, _ := utf8.DecodeRuneInString(local)
	return string(first) + "***@" + domain
}

func (rv *reviewer) links(entry *schema.SessionEntry, text string) []span {
	var spans []span
	for _, loc := range urlPattern.FindAllStringIndex(text, -1) {
		parsed, err := url.Parse(strings.TrimRight(text[loc[0]:loc[1]], ".,;"))
		if err != nil {
			continue
		}
		host := strings.ToLower(parsed.Hostname())
		target := parsed.EscapedPath()
		if parsed.RawQuery != "" {
			target += "?" + parsed.RawQuery
		}
		index := slices.IndexFunc(ShareableHosts, func(h ShareableHost) bool { return h.matches(host, target) })
		if index < 0 {
			continue
		}
		rv.add(newFinding(entry, KindShareableDocumentLink, ShareableHosts[index].Rule, host+"/"+firstSegment(parsed.EscapedPath())+"/"+masked))
		spans = append(spans, span{loc[0], loc[1]})
	}
	return spans
}

func (h ShareableHost) matches(host, target string) bool {
	if strings.HasPrefix(h.Host, ".") {
		return strings.HasSuffix(host, h.Host) && h.Path.MatchString(target)
	}
	return host == h.Host && h.Path.MatchString(target)
}

func firstSegment(escapedPath string) string {
	segment, _, _ := strings.Cut(strings.TrimPrefix(escapedPath, "/"), "/")
	return segment
}

// residuePassthrough reports each residue rule that hit. The module's own
// preview is context around the match, so it is never printed: the finding
// shows the rule alone.
func (rv *reviewer) residuePassthrough(entry *schema.SessionEntry, text string, spans []span) {
	for _, warning := range rv.residue.Scan(text) {
		if warning.RuleID == redactResidueEmailRule {
			continue
		}
		if slices.ContainsFunc(spans, func(s span) bool { return warning.Location >= s.start && warning.Location < s.end }) {
			continue
		}
		rv.add(newFinding(entry, KindRedactionResidue, warning.RuleID, warning.RuleID+": "+masked))
	}
}

// clampPreview cuts a preview to redact.PreviewMaxLength bytes on a rune
// boundary.
func clampPreview(preview string) string {
	if len(preview) <= redact.PreviewMaxLength {
		return preview
	}
	end := redact.PreviewMaxLength
	for end > 0 && !utf8.RuneStart(preview[end]) {
		end--
	}
	return preview[:end]
}
