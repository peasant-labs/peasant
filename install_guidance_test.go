package peasant

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"testing"
	"unicode"

	"gopkg.in/yaml.v3"
)

//go:embed README.md docs/KICKSTART.md docs/install/arch.md docs/install/macos.md docs/install/nix.md docs/install/ubuntu.md docs/install/wsl.md
var installGuidanceProductionFiles embed.FS

//go:embed testdata/install_guidance.yaml
var installGuidanceFixtureYAML []byte

type installGuideChannel string

const (
	channelMacOSTarball installGuideChannel = "macos-tarball"
	channelDebian       installGuideChannel = "debian-package"
	channelArchTarball  installGuideChannel = "arch-tarball"
	channelNixProfile   installGuideChannel = "nix-profile"
	channelWSLDistro    installGuideChannel = "wsl-distro"
)

type installGuidanceFixture struct {
	DeclaredRows int                         `yaml:"declared_rows"`
	Guides       []installGuidanceFixtureRow `yaml:"guides"`
}

type installGuidanceFixtureRow struct {
	Name             string              `yaml:"name"`
	Path             string              `yaml:"path"`
	Channel          installGuideChannel `yaml:"channel"`
	VersionCommand   string              `yaml:"version_command"`
	KickstartCommand string              `yaml:"kickstart_command"`
	KickstartLink    string              `yaml:"kickstart_link"`
}

var expectedInstallGuides = map[string]installGuideChannel{
	"docs/install/macos.md":  channelMacOSTarball,
	"docs/install/ubuntu.md": channelDebian,
	"docs/install/arch.md":   channelArchTarball,
	"docs/install/nix.md":    channelNixProfile,
	"docs/install/wsl.md":    channelWSLDistro,
}

const (
	installGuideRowCount            = 5
	expectedInstallVersionCommand   = "peasant version"
	expectedInstallKickstartCommand = "peasant kickstart"
	expectedInstallKickstartLink    = "../KICKSTART.md#reset-and-standalone-boundaries"
)

func decodeInstallGuidanceFixture(raw []byte) (installGuidanceFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)

	var fixture installGuidanceFixture
	if err := decoder.Decode(&fixture); err != nil {
		return installGuidanceFixture{}, fmt.Errorf("decode install guidance fixture: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return installGuidanceFixture{}, fmt.Errorf("install guidance fixture must contain exactly one YAML document")
		}
		return installGuidanceFixture{}, fmt.Errorf("decode trailing install guidance fixture document: %w", err)
	}

	if fixture.DeclaredRows != len(fixture.Guides) || fixture.DeclaredRows != installGuideRowCount || len(expectedInstallGuides) != installGuideRowCount {
		return installGuidanceFixture{}, fmt.Errorf("install guidance fixture must contain exactly %d guarded rows: declared=%d actual=%d", installGuideRowCount, fixture.DeclaredRows, len(fixture.Guides))
	}

	seenNames := make(map[string]struct{}, len(fixture.Guides))
	seenPaths := make(map[string]struct{}, len(fixture.Guides))
	for _, row := range fixture.Guides {
		if strings.TrimSpace(row.Name) == "" || strings.TrimSpace(row.Path) == "" {
			return installGuidanceFixture{}, fmt.Errorf("install guidance fixture contains a blank guide name or path")
		}
		if _, exists := seenNames[row.Name]; exists {
			return installGuidanceFixture{}, fmt.Errorf("install guidance fixture guide name %q is duplicated", row.Name)
		}
		if _, exists := seenPaths[row.Path]; exists {
			return installGuidanceFixture{}, fmt.Errorf("install guidance fixture guide path %q is duplicated", row.Path)
		}
		seenNames[row.Name] = struct{}{}
		seenPaths[row.Path] = struct{}{}

		wantChannel, knownPath := expectedInstallGuides[row.Path]
		if !knownPath {
			return installGuidanceFixture{}, fmt.Errorf("install guidance fixture names unsupported guide path %q", row.Path)
		}
		if row.Channel != wantChannel {
			return installGuidanceFixture{}, fmt.Errorf("install guidance fixture guide %q declares channel %q, want %q", row.Path, row.Channel, wantChannel)
		}
		if row.VersionCommand != expectedInstallVersionCommand || row.KickstartCommand != expectedInstallKickstartCommand {
			return installGuidanceFixture{}, fmt.Errorf("install guidance fixture guide %q must use the production commands %q then %q", row.Path, expectedInstallVersionCommand, expectedInstallKickstartCommand)
		}
		if row.KickstartLink != expectedInstallKickstartLink {
			return installGuidanceFixture{}, fmt.Errorf("install guidance fixture guide %q must link the kickstart reset boundary %q", row.Path, expectedInstallKickstartLink)
		}
	}
	return fixture, nil
}

func TestInstallGuidanceFixtureIsStrict(t *testing.T) {
	t.Parallel()

	if _, err := decodeInstallGuidanceFixture(installGuidanceFixtureYAML); err != nil {
		t.Fatalf("canonical install guidance fixture: %v", err)
	}

	unknownField := append([]byte("unexpected_fixture_field: true\n"), installGuidanceFixtureYAML...)
	if _, err := decodeInstallGuidanceFixture(unknownField); err == nil {
		t.Fatal("install guidance fixture accepted an unknown field")
	}

	trailingDocument := append(append([]byte{}, installGuidanceFixtureYAML...), []byte("\n---\nunexpected: document\n")...)
	if _, err := decodeInstallGuidanceFixture(trailingDocument); err == nil {
		t.Fatal("install guidance fixture accepted a trailing document")
	}
}

func TestInstallGuidanceProductionFiles(t *testing.T) {
	t.Parallel()

	fixture, err := decodeInstallGuidanceFixture(installGuidanceFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}

	for _, row := range fixture.Guides {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			contentBytes, err := installGuidanceProductionFiles.ReadFile(row.Path)
			if err != nil {
				t.Fatalf("read handwritten install guide %q: %v", row.Path, err)
			}
			content := string(contentBytes)
			assertVersionLeadsToKickstart(t, content, row)
			assertReinstallGuidance(t, content, row)
			assertKickstartLink(t, content, row)
		})
	}

	readme, err := installGuidanceProductionFiles.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	readmeText := strings.ToLower(string(readme))
	if !strings.Contains(readmeText, expectedInstallKickstartCommand) {
		t.Fatalf("README no longer names %q in its quick-start path", expectedInstallKickstartCommand)
	}
	if !strings.Contains(readmeText, "docs/kickstart.md") {
		t.Fatal("README no longer links the kickstart guide")
	}
}

func assertVersionLeadsToKickstart(t *testing.T, content string, row installGuidanceFixtureRow) {
	t.Helper()
	lower := strings.ToLower(content)
	versionIndex := strings.Index(lower, strings.ToLower(row.VersionCommand))
	if versionIndex < 0 {
		t.Fatalf("guide does not show the version verification command %q", row.VersionCommand)
	}
	kickstartIndex := strings.Index(lower, strings.ToLower(row.KickstartCommand))
	if kickstartIndex < 0 {
		t.Fatalf("guide does not show the guided setup command %q", row.KickstartCommand)
	}
	if versionIndex >= kickstartIndex {
		t.Fatalf("guide puts %q before version verification %q", row.KickstartCommand, row.VersionCommand)
	}

	start := kickstartIndex - 240
	if start < 0 {
		start = 0
	}
	end := kickstartIndex + len(row.KickstartCommand) + 240
	if end > len(lower) {
		end = len(lower)
	}
	context := lower[start:end]
	for _, marker := range []string{"guided", "wizard"} {
		if !strings.Contains(context, marker) {
			t.Fatalf("guide's next-step context around %q does not identify the guided setup wizard; missing %q", row.KickstartCommand, marker)
		}
	}
}

func assertReinstallGuidance(t *testing.T, content string, row installGuidanceFixtureRow) {
	t.Helper()
	section := findInstallGuidanceSection(content, "reinstall", "upgrad")
	if section == "" {
		t.Fatal("guide has no section that explains reinstalling and upgrading")
	}
	lower := strings.ToLower(section)
	for _, marker := range []string{"reinstall", "upgrad", "config", "data"} {
		if !strings.Contains(lower, marker) {
			t.Fatalf("reinstall/upgrade section is missing the non-destructive %q behavior", marker)
		}
	}
	if !strings.Contains(lower, "does not") && !strings.Contains(lower, "do not") && !strings.Contains(lower, "without") {
		t.Fatal("reinstall/upgrade section does not say that replacement leaves existing state in place")
	}

	var required []string
	switch row.Channel {
	case channelMacOSTarball, channelArchTarball:
		required = []string{"tar.gz", "sudo install"}
	case channelDebian:
		required = []string{".deb", "apt install"}
	case channelNixProfile:
		required = []string{"nix profile install", "nix profile upgrade"}
	case channelWSLDistro:
		required = []string{"underlying distro", "apt install", "tarball"}
	default:
		t.Fatalf("unknown install channel %q", row.Channel)
	}
	for _, marker := range required {
		if !strings.Contains(lower, marker) {
			t.Fatalf("%s reinstall/upgrade section is missing channel marker %q", row.Channel, marker)
		}
	}
}

func findInstallGuidanceSection(content string, markers ...string) string {
	lines := strings.Split(content, "\n")
	start := -1
	for index, line := range lines {
		trimmed := strings.ToLower(strings.TrimSpace(line))
		if !strings.HasPrefix(trimmed, "## ") {
			continue
		}
		matches := true
		for _, marker := range markers {
			if !strings.Contains(trimmed, marker) {
				matches = false
				break
			}
		}
		if matches {
			start = index
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := len(lines)
	for index := start + 1; index < len(lines); index++ {
		if strings.HasPrefix(strings.TrimSpace(lines[index]), "## ") {
			end = index
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

func assertKickstartLink(t *testing.T, content string, row installGuidanceFixtureRow) {
	t.Helper()
	if !strings.Contains(content, "]("+row.KickstartLink+")") {
		t.Fatalf("guide does not link the kickstart rerun/reset boundary %q", row.KickstartLink)
	}

	targetPath, fragment, _ := strings.Cut(row.KickstartLink, "#")
	if targetPath == "" || fragment == "" {
		t.Fatalf("fixture link %q must include a relative target and a fragment", row.KickstartLink)
	}
	resolved := path.Clean(path.Join(path.Dir(row.Path), targetPath))
	if resolved == "docs" || !strings.HasPrefix(resolved, "docs/") {
		t.Fatalf("fixture link %q escapes the docs tree", row.KickstartLink)
	}
	targetBytes, err := installGuidanceProductionFiles.ReadFile(resolved)
	if err != nil {
		t.Fatalf("resolve kickstart link %q from %q: %v", row.KickstartLink, row.Path, err)
	}
	target := string(targetBytes)
	if !hasMarkdownFragment(target, fragment) {
		t.Fatalf("kickstart link %q points to a missing heading anchor", row.KickstartLink)
	}
	lowerTarget := strings.ToLower(target)
	for _, marker := range []string{"peasant kickstart", "--reset", "without", "preserves"} {
		if !strings.Contains(lowerTarget, marker) {
			t.Fatalf("kickstart reset boundary is missing the rerun marker %q", marker)
		}
	}
}

func hasMarkdownFragment(content, fragment string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") {
			continue
		}
		heading := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		if markdownHeadingSlug(heading) == fragment {
			return true
		}
	}
	return false
}

func markdownHeadingSlug(heading string) string {
	var slug strings.Builder
	dashPending := false
	for _, r := range strings.ToLower(strings.TrimSpace(heading)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			slug.WriteRune(r)
			dashPending = false
			continue
		}
		if slug.Len() > 0 {
			dashPending = true
		}
		if dashPending && !strings.HasSuffix(slug.String(), "-") {
			slug.WriteByte('-')
		}
	}
	return strings.Trim(slug.String(), "-")
}
