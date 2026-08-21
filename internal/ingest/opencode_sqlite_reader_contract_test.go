package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
)

const (
	expectedReaderPageCases          = 3
	expectedReaderInvalidIdentifiers = 1
	expectedReaderMethods            = 13
	expectedReaderSignatureRules     = 5
	expectedReaderGuardMutations     = 5
	expectedReaderLoaderMutations    = 4
)

//go:embed testdata/opencode_sqlite_reader.yaml
var openCodeSQLiteReaderYAML []byte

type readerPageKind string

const (
	readerPageSessions readerPageKind = "sessions"
	readerPageMessages readerPageKind = "messages"
	readerPageParts    readerPageKind = "parts"
)

type readerInvalidIdentifierKind string

const readerInvalidPartID readerInvalidIdentifierKind = "part"

type readerSignatureRuleKind string

const (
	readerRuleContains     readerSignatureRuleKind = "contains"
	readerRuleNestedFunc   readerSignatureRuleKind = "nested_func"
	readerRuleDirectString readerSignatureRuleKind = "direct_string"
	readerRuleStringID     readerSignatureRuleKind = "string_id"
)

type readerGuardMutationKind string

const (
	readerMutationRawHandle        readerGuardMutationKind = "raw_handle"
	readerMutationGenericArguments readerGuardMutationKind = "generic_arguments"
	readerMutationCallback         readerGuardMutationKind = "callback"
	readerMutationBareString       readerGuardMutationKind = "bare_string"
	readerMutationStringID         readerGuardMutationKind = "string_id"
)

type readerLoaderMutationKind string

const (
	readerLoaderUnknownField    readerLoaderMutationKind = "unknown_field"
	readerLoaderTrailingDoc     readerLoaderMutationKind = "trailing_document"
	readerLoaderDeclaredCount   readerLoaderMutationKind = "declared_count"
	readerLoaderDuplicateMethod readerLoaderMutationKind = "duplicate_method"
)

type readerContractFixture struct {
	DeclaredPageCases          int                           `yaml:"declared_page_cases"`
	PageCases                  []readerPageCase              `yaml:"page_cases"`
	DeclaredInvalidIdentifiers int                           `yaml:"declared_invalid_identifiers"`
	InvalidIdentifiers         []readerInvalidIdentifierCase `yaml:"invalid_identifiers"`
	DeclaredMethods            int                           `yaml:"declared_methods"`
	Methods                    []readerMethod                `yaml:"methods"`
	DeclaredSignatureRules     int                           `yaml:"declared_signature_rules"`
	SignatureRules             []readerSignatureRule         `yaml:"signature_rules"`
	DeclaredGuardMutations     int                           `yaml:"declared_guard_mutations"`
	GuardMutations             []readerGuardMutation         `yaml:"guard_mutations"`
	DeclaredLoaderMutations    int                           `yaml:"declared_loader_mutations"`
	LoaderMutations            []readerLoaderMutation        `yaml:"loader_mutations"`
}

type readerPageCase struct {
	Name      string               `yaml:"name"`
	Kind      readerPageKind       `yaml:"kind"`
	Fixture   string               `yaml:"fixture"`
	SessionID string               `yaml:"session_id"`
	MessageID string               `yaml:"message_id"`
	PageSize  int                  `yaml:"page_size"`
	Pages     []readerExpectedPage `yaml:"pages"`
}

type readerExpectedPage struct {
	IDs     []string `yaml:"ids"`
	HasNext bool     `yaml:"has_next"`
}

type readerInvalidIdentifierCase struct {
	Name          string                      `yaml:"name"`
	Kind          readerInvalidIdentifierKind `yaml:"kind"`
	Value         string                      `yaml:"value"`
	ErrorContains string                      `yaml:"error_contains"`
}

type readerMethod struct {
	Name      string `yaml:"name"`
	Signature string `yaml:"signature"`
}

type readerSignatureRule struct {
	Name  string                  `yaml:"name"`
	Kind  readerSignatureRuleKind `yaml:"kind"`
	Token string                  `yaml:"token"`
}

type readerGuardMutation struct {
	Name          string                  `yaml:"name"`
	Kind          readerGuardMutationKind `yaml:"kind"`
	ErrorContains string                  `yaml:"error_contains"`
}

type readerLoaderMutation struct {
	Name          string                   `yaml:"name"`
	Kind          readerLoaderMutationKind `yaml:"kind"`
	ErrorContains string                   `yaml:"error_contains"`
}

func TestOpenCodeLegacyReaderPagesMatchStrictFixture(t *testing.T) {
	fixture := loadReaderContractFixture(t)
	for _, fixtureCase := range fixture.PageCases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, fixtureCase.Fixture)
			source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
			defer closeSyntheticSource(t, source)
			pageSize := mustLegacyPageSize(t, fixtureCase.PageSize)
			switch fixtureCase.Kind {
			case readerPageSessions:
				assertFixturePages(t, fixtureCase, func(cursor *ingest.OpenCodeLegacySessionCursor) (readerFetchedPage[ingest.OpenCodeLegacySessionCursor], error) {
					page, err := source.LegacySessionIDs(t.Context(), ingest.OpenCodeLegacySessionPageRequest{PageSize: pageSize, After: cursor})
					return readerFetchedPage[ingest.OpenCodeLegacySessionCursor]{IDs: legacySessionStrings(page.SessionIDs), Capacity: cap(page.SessionIDs), Next: page.Next}, err
				})
			case readerPageMessages:
				sessionID := mustLegacySessionID(t, fixtureCase.SessionID)
				assertFixturePages(t, fixtureCase, func(cursor *ingest.OpenCodeLegacyMessageCursor) (readerFetchedPage[ingest.OpenCodeLegacyMessageCursor], error) {
					page, err := source.LegacyMessages(t.Context(), ingest.OpenCodeLegacyMessagePageRequest{SessionID: sessionID, PageSize: pageSize, After: cursor})
					return readerFetchedPage[ingest.OpenCodeLegacyMessageCursor]{IDs: legacyMessageStrings(page.Messages), Capacity: cap(page.Messages), Next: page.Next}, err
				})
			case readerPageParts:
				sessionID := mustLegacySessionID(t, fixtureCase.SessionID)
				messageID := mustLegacyMessageID(t, fixtureCase.MessageID)
				assertFixturePages(t, fixtureCase, func(cursor *ingest.OpenCodeLegacyPartCursor) (readerFetchedPage[ingest.OpenCodeLegacyPartCursor], error) {
					page, err := source.LegacyParts(t.Context(), ingest.OpenCodeLegacyPartPageRequest{SessionID: sessionID, MessageID: messageID, PageSize: pageSize, After: cursor})
					return readerFetchedPage[ingest.OpenCodeLegacyPartCursor]{IDs: legacyPartStrings(page.Parts), Capacity: cap(page.Parts), Next: page.Next}, err
				})
			default:
				t.Fatalf("strict reader fixture %q has unsupported page kind %q", fixtureCase.Name, fixtureCase.Kind)
			}
		})
	}
}

type readerFetchedPage[C any] struct {
	IDs      []string
	Capacity int
	Next     *C
}

func assertFixturePages[C any](t testing.TB, fixtureCase readerPageCase, fetch func(*C) (readerFetchedPage[C], error)) {
	t.Helper()
	var cursor *C
	seen := make(map[string]struct{})
	for index, expected := range fixtureCase.Pages {
		requestCursor := cursor
		page, err := fetch(requestCursor)
		if err != nil {
			t.Fatalf("read strict fixture page %d: %v", index, err)
		}
		assertFixturePage(t, fixtureCase, index, page.IDs, page.Capacity, page.Next != nil, expected, seen)
		repeated, err := fetch(requestCursor)
		if err != nil {
			t.Fatalf("repeat strict fixture cursor for page %d: %v", index, err)
		}
		if !equalStrings(repeated.IDs, page.IDs) || repeated.Capacity != page.Capacity || (repeated.Next != nil) != (page.Next != nil) {
			t.Fatalf("repeated strict fixture page %d = ids %v cap %d next %t, want deterministic ids %v cap %d next %t", index, repeated.IDs, repeated.Capacity, repeated.Next != nil, page.IDs, page.Capacity, page.Next != nil)
		}
		cursor = page.Next
	}
	if cursor != nil {
		t.Fatalf("strict fixture %q ended with continuation cursor after final page", fixtureCase.Name)
	}
}

func assertFixturePage(t testing.TB, fixtureCase readerPageCase, index int, ids []string, capacity int, hasNext bool, expected readerExpectedPage, seen map[string]struct{}) {
	t.Helper()
	if !equalStrings(ids, expected.IDs) {
		t.Fatalf("strict fixture page %d IDs = %v, want %v", index, ids, expected.IDs)
	}
	if len(ids) != len(expected.IDs) || capacity != len(ids) {
		t.Fatalf("strict fixture page %d length/capacity = %d/%d, want exact returned bound %d/%d so no sentinel can be resliced", index, len(ids), capacity, len(expected.IDs), len(expected.IDs))
	}
	if len(ids) > fixtureCase.PageSize {
		t.Fatalf("strict fixture page %d returned %d rows above requested size %d", index, len(ids), fixtureCase.PageSize)
	}
	if hasNext != expected.HasNext {
		t.Fatalf("strict fixture page %d continuation = %t, want %t", index, hasNext, expected.HasNext)
	}
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("strict fixture page %d repeated sentinel or previously returned identity %q", index, id)
		}
		seen[id] = struct{}{}
	}
}

func TestOpenCodeLegacyPartIDRejectsStrictFixtureCases(t *testing.T) {
	fixture := loadReaderContractFixture(t)
	for _, fixtureCase := range fixture.InvalidIdentifiers {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			if fixtureCase.Kind != readerInvalidPartID {
				t.Fatalf("strict reader fixture %q has unsupported invalid identifier kind %q", fixtureCase.Name, fixtureCase.Kind)
			}
			_, err := ingest.NewOpenCodeLegacyPartID(fixtureCase.Value)
			if err == nil || !strings.Contains(err.Error(), fixtureCase.ErrorContains) {
				t.Fatalf("invalid part identifier error = %v, want substring %q", err, fixtureCase.ErrorContains)
			}
		})
	}
}

func TestOpenCodeSQLiteSourceSurfaceMatchesStrictFixture(t *testing.T) {
	fixture := loadReaderContractFixture(t)
	interfaceType := reflect.TypeOf((*ingest.OpenCodeSQLiteSource)(nil)).Elem()
	if err := validateReaderMethodInventory(interfaceType, fixture.Methods); err != nil {
		t.Fatal(err)
	}
	if err := validateReaderSurfaceTypes(interfaceType, fixture.SignatureRules); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeSQLiteSourceSurfaceGuardMutationsAreNonVacuous(t *testing.T) {
	fixture := loadReaderContractFixture(t)
	for _, mutation := range fixture.GuardMutations {
		mutation := mutation
		t.Run(mutation.Name, func(t *testing.T) {
			interfaceType := readerMutationInterface(t, mutation.Kind)
			err := validateReaderSurfaceTypes(interfaceType, fixture.SignatureRules)
			if err == nil || !strings.Contains(err.Error(), mutation.ErrorContains) {
				t.Fatalf("surface guard mutation error = %v, want rule %q", err, mutation.ErrorContains)
			}
		})
	}
}

func TestOpenCodeSQLiteReaderFixtureLoaderMutationsAreRejected(t *testing.T) {
	fixture := loadReaderContractFixture(t)
	for _, mutation := range fixture.LoaderMutations {
		mutation := mutation
		t.Run(mutation.Name, func(t *testing.T) {
			mutated, err := mutateReaderFixture(openCodeSQLiteReaderYAML, mutation.Kind)
			if err != nil {
				t.Fatalf("apply strict reader fixture mutation: %v", err)
			}
			_, err = parseReaderContractFixture(mutated)
			if err == nil || !strings.Contains(err.Error(), mutation.ErrorContains) {
				t.Fatalf("strict reader fixture mutation error = %v, want substring %q", err, mutation.ErrorContains)
			}
		})
	}
}

func validateReaderMethodInventory(interfaceType reflect.Type, expected []readerMethod) error {
	if interfaceType.Kind() != reflect.Interface {
		return fmt.Errorf("validate OpenCode SQLite source method inventory failed: production type %s is not an interface; callers cannot rely on the restrictive contract; restore OpenCodeSQLiteSource as an interface", interfaceType)
	}
	if interfaceType.NumMethod() != len(expected) {
		return fmt.Errorf("validate OpenCode SQLite source method inventory failed: production exposes %d methods but strict fixture declares %d; the boundary may have drifted; update the typed interface and reviewed fixture together", interfaceType.NumMethod(), len(expected))
	}
	expectedByName := make(map[string]string, len(expected))
	for _, method := range expected {
		expectedByName[method.Name] = method.Signature
	}
	for index := 0; index < interfaceType.NumMethod(); index++ {
		method := interfaceType.Method(index)
		want, ok := expectedByName[method.Name]
		if !ok {
			return fmt.Errorf("validate OpenCode SQLite source method inventory failed: production method %q is absent from the strict fixture; an unreviewed operation could escape the bounded boundary; add only a typed reviewed method and fixture entry", method.Name)
		}
		if got := method.Type.String(); got != want {
			return fmt.Errorf("validate OpenCode SQLite source method inventory failed for %q: signature is %q, want strict fixture signature %q; raw or generic types may have escaped; restore the typed signature or update the reviewed fixture", method.Name, got, want)
		}
		delete(expectedByName, method.Name)
	}
	if len(expectedByName) != 0 {
		return fmt.Errorf("validate OpenCode SQLite source method inventory failed: strict fixture methods %v are missing from production; mounted consumers cannot use the promised contract; restore the methods", expectedByName)
	}
	return nil
}

func validateReaderSurfaceTypes(interfaceType reflect.Type, rules []readerSignatureRule) error {
	for index := 0; index < interfaceType.NumMethod(); index++ {
		method := interfaceType.Method(index)
		for _, rule := range rules {
			switch rule.Kind {
			case readerRuleContains:
				if path, found := findReaderTypeToken(method.Type, method.Name, rule.Token, make(map[reflect.Type]bool)); found {
					return fmt.Errorf("reader surface rule %s rejected method %s because type path %s contains forbidden token %q", rule.Name, method.Name, path, rule.Token)
				}
			case readerRuleNestedFunc:
				if path, found := findReaderCallback(method.Type, method.Name, rule.Token, true, make(map[reflect.Type]bool)); found {
					return fmt.Errorf("reader surface rule %s rejected method %s because type path %s contains a callback", rule.Name, method.Name, path)
				}
			case readerRuleDirectString:
				for argument := 0; argument < method.Type.NumIn(); argument++ {
					if path, found := findReaderBareInputString(method.Type.In(argument), fmt.Sprintf("%s.in%d", method.Name, argument), rule.Token, make(map[reflect.Type]bool)); found {
						return fmt.Errorf("reader surface rule %s rejected method %s because input path %s is a bare string that could carry SQL or a table name", rule.Name, method.Name, path)
					}
				}
			case readerRuleStringID:
				if path, found := findStringlyIdentity(method.Type, method.Name, rule.Token, make(map[reflect.Type]bool)); found {
					return fmt.Errorf("reader surface rule %s rejected method %s because identity path %s is a bare string", rule.Name, method.Name, path)
				}
			default:
				return fmt.Errorf("reader surface validation encountered unknown strict rule kind %q after fixture validation", rule.Kind)
			}
		}
	}
	return nil
}

func findReaderTypeToken(value reflect.Type, path, token string, seen map[reflect.Type]bool) (string, bool) {
	if strings.Contains(value.String(), token) {
		return path, true
	}
	if seen[value] {
		return "", false
	}
	seen[value] = true
	switch value.Kind() {
	case reflect.Func:
		for index := 0; index < value.NumIn(); index++ {
			if foundPath, found := findReaderTypeToken(value.In(index), fmt.Sprintf("%s.in%d", path, index), token, seen); found {
				return foundPath, true
			}
		}
		for index := 0; index < value.NumOut(); index++ {
			if foundPath, found := findReaderTypeToken(value.Out(index), fmt.Sprintf("%s.out%d", path, index), token, seen); found {
				return foundPath, true
			}
		}
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return findReaderTypeToken(value.Elem(), path, token, seen)
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			field := value.Field(index)
			if foundPath, found := findReaderTypeToken(field.Type, path+"."+field.Name, token, seen); found {
				return foundPath, true
			}
		}
	}
	return "", false
}

func findReaderCallback(value reflect.Type, path, token string, root bool, seen map[reflect.Type]bool) (string, bool) {
	if value.Kind() == reflect.Func && !root && strings.Contains(value.String(), token) {
		return path, true
	}
	if seen[value] {
		return "", false
	}
	seen[value] = true
	switch value.Kind() {
	case reflect.Func:
		for index := 0; index < value.NumIn(); index++ {
			if foundPath, found := findReaderCallback(value.In(index), fmt.Sprintf("%s.in%d", path, index), token, false, seen); found {
				return foundPath, true
			}
		}
		for index := 0; index < value.NumOut(); index++ {
			if foundPath, found := findReaderCallback(value.Out(index), fmt.Sprintf("%s.out%d", path, index), token, false, seen); found {
				return foundPath, true
			}
		}
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return findReaderCallback(value.Elem(), path, token, false, seen)
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			field := value.Field(index)
			if foundPath, found := findReaderCallback(field.Type, path+"."+field.Name, token, false, seen); found {
				return foundPath, true
			}
		}
	}
	return "", false
}

func findReaderBareInputString(value reflect.Type, path, token string, seen map[reflect.Type]bool) (string, bool) {
	if value.Kind() == reflect.String && value.Kind().String() == token {
		return path, true
	}
	if seen[value] {
		return "", false
	}
	seen[value] = true
	switch value.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return findReaderBareInputString(value.Elem(), path, token, seen)
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			field := value.Field(index)
			if field.PkgPath != "" {
				continue
			}
			if foundPath, found := findReaderBareInputString(field.Type, path+"."+field.Name, token, seen); found {
				return foundPath, true
			}
		}
	}
	return "", false
}

func findStringlyIdentity(value reflect.Type, path, identityToken string, seen map[reflect.Type]bool) (string, bool) {
	if seen[value] {
		return "", false
	}
	seen[value] = true
	switch value.Kind() {
	case reflect.Func:
		for index := 0; index < value.NumIn(); index++ {
			if foundPath, found := findStringlyIdentity(value.In(index), fmt.Sprintf("%s.in%d", path, index), identityToken, seen); found {
				return foundPath, true
			}
		}
		for index := 0; index < value.NumOut(); index++ {
			if foundPath, found := findStringlyIdentity(value.Out(index), fmt.Sprintf("%s.out%d", path, index), identityToken, seen); found {
				return foundPath, true
			}
		}
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return findStringlyIdentity(value.Elem(), path, identityToken, seen)
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			field := value.Field(index)
			fieldPath := path + "." + field.Name
			if strings.HasSuffix(strings.ToLower(field.Name), strings.ToLower(identityToken)) && field.Type.Kind() == reflect.String {
				return fieldPath, true
			}
			if foundPath, found := findStringlyIdentity(field.Type, fieldPath, identityToken, seen); found {
				return foundPath, true
			}
		}
	}
	return "", false
}

type readerRawHandleMutation interface {
	Escape(context.Context, *sqlite.Conn) error
}

type readerGenericArgumentsMutation interface {
	Escape(context.Context, []any) error
}

type readerCallbackMutation interface {
	Escape(context.Context, func()) error
}

type readerBareStringMutation interface {
	Escape(context.Context, string) error
}

type readerStringIDMutationRequest struct {
	PartID string
}

type readerStringIDMutation interface {
	Escape(context.Context, readerStringIDMutationRequest) error
}

func readerMutationInterface(t testing.TB, kind readerGuardMutationKind) reflect.Type {
	t.Helper()
	switch kind {
	case readerMutationRawHandle:
		return reflect.TypeOf((*readerRawHandleMutation)(nil)).Elem()
	case readerMutationGenericArguments:
		return reflect.TypeOf((*readerGenericArgumentsMutation)(nil)).Elem()
	case readerMutationCallback:
		return reflect.TypeOf((*readerCallbackMutation)(nil)).Elem()
	case readerMutationBareString:
		return reflect.TypeOf((*readerBareStringMutation)(nil)).Elem()
	case readerMutationStringID:
		return reflect.TypeOf((*readerStringIDMutation)(nil)).Elem()
	default:
		t.Fatalf("strict reader fixture has unsupported guard mutation kind %q", kind)
		return nil
	}
}

func loadReaderContractFixture(t testing.TB) readerContractFixture {
	t.Helper()
	fixture, err := parseReaderContractFixture(openCodeSQLiteReaderYAML)
	if err != nil {
		t.Fatalf("load strict OpenCode SQLite reader fixture: %v", err)
	}
	return fixture
}

func parseReaderContractFixture(data []byte) (readerContractFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixture readerContractFixture
	if err := decoder.Decode(&fixture); err != nil {
		return readerContractFixture{}, fmt.Errorf("decode strict OpenCode SQLite reader fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return readerContractFixture{}, fmt.Errorf("decode strict OpenCode SQLite reader fixture: expected exactly one YAML document: %w", err)
	}
	if fixture.DeclaredPageCases != expectedReaderPageCases || len(fixture.PageCases) != expectedReaderPageCases ||
		fixture.DeclaredInvalidIdentifiers != expectedReaderInvalidIdentifiers || len(fixture.InvalidIdentifiers) != expectedReaderInvalidIdentifiers ||
		fixture.DeclaredMethods != expectedReaderMethods || len(fixture.Methods) != expectedReaderMethods ||
		fixture.DeclaredSignatureRules != expectedReaderSignatureRules || len(fixture.SignatureRules) != expectedReaderSignatureRules ||
		fixture.DeclaredGuardMutations != expectedReaderGuardMutations || len(fixture.GuardMutations) != expectedReaderGuardMutations ||
		fixture.DeclaredLoaderMutations != expectedReaderLoaderMutations || len(fixture.LoaderMutations) != expectedReaderLoaderMutations {
		return readerContractFixture{}, fmt.Errorf("strict OpenCode SQLite reader fixture row guard failed: pages=%d/%d invalid=%d/%d methods=%d/%d rules=%d/%d guard_mutations=%d/%d loader_mutations=%d/%d", fixture.DeclaredPageCases, len(fixture.PageCases), fixture.DeclaredInvalidIdentifiers, len(fixture.InvalidIdentifiers), fixture.DeclaredMethods, len(fixture.Methods), fixture.DeclaredSignatureRules, len(fixture.SignatureRules), fixture.DeclaredGuardMutations, len(fixture.GuardMutations), fixture.DeclaredLoaderMutations, len(fixture.LoaderMutations))
	}
	if err := validateReaderFixture(fixture); err != nil {
		return readerContractFixture{}, err
	}
	return fixture, nil
}

func validateReaderFixture(fixture readerContractFixture) error {
	names := make(map[string]string)
	for _, fixtureCase := range fixture.PageCases {
		if err := addReaderFixtureName(names, "page", fixtureCase.Name); err != nil {
			return err
		}
		if fixtureCase.Fixture == "" || fixtureCase.PageSize <= 0 || len(fixtureCase.Pages) < 2 {
			return fmt.Errorf("strict reader page fixture is incomplete: %+v", fixtureCase)
		}
		for index, page := range fixtureCase.Pages {
			if len(page.IDs) == 0 || len(page.IDs) > fixtureCase.PageSize || page.HasNext != (index < len(fixtureCase.Pages)-1) {
				return fmt.Errorf("strict reader page fixture %q page %d has invalid IDs or continuation: %+v", fixtureCase.Name, index, page)
			}
		}
	}
	for _, fixtureCase := range fixture.InvalidIdentifiers {
		if err := addReaderFixtureName(names, "invalid_identifier", fixtureCase.Name); err != nil {
			return err
		}
		if fixtureCase.Kind != readerInvalidPartID || fixtureCase.Value == "" || fixtureCase.ErrorContains == "" {
			return fmt.Errorf("strict invalid identifier fixture is incomplete: %+v", fixtureCase)
		}
	}
	for _, method := range fixture.Methods {
		if err := addReaderFixtureName(names, "method", method.Name); err != nil {
			return err
		}
		if method.Signature == "" {
			return fmt.Errorf("strict reader method %q has an empty signature", method.Name)
		}
	}
	for _, rule := range fixture.SignatureRules {
		if err := addReaderFixtureName(names, "rule", rule.Name); err != nil {
			return err
		}
		if rule.Token == "" || !knownReaderRule(rule.Kind) {
			return fmt.Errorf("strict reader signature rule is incomplete: %+v", rule)
		}
	}
	for _, mutation := range fixture.GuardMutations {
		if err := addReaderFixtureName(names, "guard_mutation", mutation.Name); err != nil {
			return err
		}
		if mutation.ErrorContains == "" || !knownReaderMutation(mutation.Kind) {
			return fmt.Errorf("strict reader guard mutation is incomplete: %+v", mutation)
		}
	}
	for _, mutation := range fixture.LoaderMutations {
		if err := addReaderFixtureName(names, "loader_mutation", mutation.Name); err != nil {
			return err
		}
		if mutation.ErrorContains == "" || !knownReaderLoaderMutation(mutation.Kind) {
			return fmt.Errorf("strict reader loader mutation is incomplete: %+v", mutation)
		}
	}
	return nil
}

func addReaderFixtureName(names map[string]string, group, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("strict OpenCode SQLite reader fixture %s has an empty name", group)
	}
	key := group + "\x00" + name
	if prior, duplicate := names[key]; duplicate {
		return fmt.Errorf("strict OpenCode SQLite reader fixture %s duplicates name %q previously recorded in %s", group, name, prior)
	}
	names[key] = group
	return nil
}

func knownReaderRule(kind readerSignatureRuleKind) bool {
	switch kind {
	case readerRuleContains, readerRuleNestedFunc, readerRuleDirectString, readerRuleStringID:
		return true
	default:
		return false
	}
}

func knownReaderMutation(kind readerGuardMutationKind) bool {
	switch kind {
	case readerMutationRawHandle, readerMutationGenericArguments, readerMutationCallback, readerMutationBareString, readerMutationStringID:
		return true
	default:
		return false
	}
}

func knownReaderLoaderMutation(kind readerLoaderMutationKind) bool {
	switch kind {
	case readerLoaderUnknownField, readerLoaderTrailingDoc, readerLoaderDeclaredCount, readerLoaderDuplicateMethod:
		return true
	default:
		return false
	}
}

func mutateReaderFixture(source []byte, kind readerLoaderMutationKind) ([]byte, error) {
	replaceOnce := func(old, replacement string) ([]byte, error) {
		if !bytes.Contains(source, []byte(old)) {
			return nil, fmt.Errorf("strict reader fixture mutation anchor %q is absent", old)
		}
		return bytes.Replace(source, []byte(old), []byte(replacement), 1), nil
	}
	switch kind {
	case readerLoaderUnknownField:
		return append(append([]byte(nil), source...), []byte("unexpected: true\n")...), nil
	case readerLoaderTrailingDoc:
		return append(append([]byte(nil), source...), []byte("---\ndeclared_page_cases: 0\n")...), nil
	case readerLoaderDeclaredCount:
		return replaceOnce("declared_methods: 13", "declared_methods: 12")
	case readerLoaderDuplicateMethod:
		return replaceOnce("  - name: Close\n", "  - name: Catalog\n")
	default:
		return nil, fmt.Errorf("unknown strict reader fixture loader mutation %q", kind)
	}
}
