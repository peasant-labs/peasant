package api

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	regexp "github.com/wasilibs/go-re2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// The Local API contract declares, for every kickstart route, each non-2xx
// status it answers and the closed error codes that status carries. The schema
// module keeps that record in testdata/local-api/kickstart_errors.yaml, apart
// from the Go type that generates the document, so the two can disagree and a
// test can notice. The script harness holds every kickstart answer it receives
// to the record: a route that answers a status or a code the contract does not
// declare for it fails the case that provoked it.

const kickstartErrorsFixture = "testdata/local-api/kickstart_errors.yaml"

type kickstartContractRoute struct {
	OperationID string              `yaml:"operation_id"`
	Method      string              `yaml:"method"`
	Path        string              `yaml:"path"`
	Errors      map[string][]string `yaml:"errors"`
	Other       []string            `yaml:"other_statuses"`
	pattern     *regexp.Regexp
}

type kickstartContract struct {
	RequiredRoutes []string                 `yaml:"required_routes"`
	Routes         []kickstartContractRoute `yaml:"routes"`
	CodeStatus     map[string]int           `yaml:"code_status"`
}

var (
	kickstartContractOnce   sync.Once
	kickstartContractValue  *kickstartContract
	kickstartContractReason string
)

// loadKickstartContract reads the schema module's record of the kickstart
// errors. It returns nil, with the reason, when the module's test data is not on
// this machine: a build from a vendor directory carries only the packages the
// binary imports. The authoritative gate (make check) builds from the module
// cache, where the record is present.
func loadKickstartContract(t testing.TB) (*kickstartContract, string) {
	t.Helper()
	kickstartContractOnce.Do(func() {
		out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/peasant-labs/schema").Output()
		dir := strings.TrimSpace(string(out))
		if err != nil || dir == "" {
			kickstartContractReason = fmt.Sprintf("the schema module's directory is not available (go list: %v)", err)
			return
		}
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(kickstartErrorsFixture)))
		if err != nil {
			kickstartContractReason = fmt.Sprintf("the schema module at %s carries no %s: %v", dir, kickstartErrorsFixture, err)
			return
		}
		var contract kickstartContract
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		if err := decoder.Decode(&contract); err != nil {
			kickstartContractReason = fmt.Sprintf("%s does not decode: %v", kickstartErrorsFixture, err)
			return
		}
		for index := range contract.Routes {
			route := &contract.Routes[index]
			expression := regexp.QuoteMeta(route.Path)
			expression = regexp.MustCompile(`\\\{[A-Za-z]+\\\}`).ReplaceAllString(expression, `[^/]+`)
			route.pattern = regexp.MustCompile("^" + expression + "$")
		}
		kickstartContractValue = &contract
	})
	return kickstartContractValue, kickstartContractReason
}

// route finds the declared route a request addresses, or nil for a request that
// is not a kickstart route.
func (c *kickstartContract) route(method, path string) *kickstartContractRoute {
	for index := range c.Routes {
		route := &c.Routes[index]
		if strings.EqualFold(route.Method, method) && route.pattern.MatchString(path) {
			return route
		}
	}
	return nil
}

// kickstartAnswerFault checks one answer a kickstart route sent against the
// contract and returns what is wrong with it, or empty. A 2xx answer is not this
// check's business. The other statuses are three kinds: the shared 403 refusal,
// the commit 409 with its rows, and the closed error body.
func kickstartAnswerFault(contract *kickstartContract, method, path string, status int, body []byte) string {
	route := contract.route(method, strings.SplitN(path, "?", 2)[0])
	if route == nil || status < 400 {
		return ""
	}
	declaredOther := func(status int) bool {
		for _, other := range route.Other {
			if other == strconv.Itoa(status) {
				return true
			}
		}
		return false
	}
	switch status {
	case http.StatusForbidden:
		if !declaredOther(status) {
			return fmt.Sprintf("%s answers 403, which the contract does not declare for it", route.OperationID)
		}
		var refusal struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(body, &refusal); err != nil {
			return fmt.Sprintf("the 403 body is not JSON: %v", err)
		}
		allowed := []string{string(codeRequestHostNotLocal), string(codeRequestOriginNotLocal)}
		if strings.EqualFold(method, http.MethodGet) {
			allowed = allowed[:1]
		}
		for _, code := range allowed {
			if refusal.Code == code {
				return ""
			}
		}
		return fmt.Sprintf("%s answers 403 with code %q, want one of %v", route.OperationID, refusal.Code, allowed)
	case http.StatusConflict:
		if !declaredOther(status) {
			return fmt.Sprintf("%s answers 409, which the contract does not declare for it", route.OperationID)
		}
		var refusal schema.LocalKickstartCommitRefusal
		if err := json.Unmarshal(body, &refusal); err != nil {
			return fmt.Sprintf("the 409 body is not a commit refusal: %v", err)
		}
		if err := refusal.Validate(); err != nil {
			return fmt.Sprintf("the 409 body breaks the contract: %v", err)
		}
		return ""
	}
	var response schema.LocalKickstartErrorResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return fmt.Sprintf("%s answers %d with a body that is not the contract's error response: %v; body: %s", route.OperationID, status, err, body)
	}
	if err := response.Validate(); err != nil {
		return fmt.Sprintf("%s answers %d with an error response that breaks the contract: %v", route.OperationID, status, err)
	}
	if want := response.Code.HTTPStatus(); want != status {
		return fmt.Sprintf("%s answers %d with code %s, which answers %d", route.OperationID, status, response.Code, want)
	}
	for _, code := range route.Errors[strconv.Itoa(status)] {
		if code == response.Code.String() {
			return ""
		}
	}
	return fmt.Sprintf("%s answers %d with code %s, which the contract does not declare for it (declared: %v)", route.OperationID, status, response.Code, route.Errors[strconv.Itoa(status)])
}

//go:embed testdata/kickstart_contract_faults.yaml
var kickstartContractFaultsYAML []byte

type kickstartContractFaultCase struct {
	Name   string `yaml:"name"`
	Method string `yaml:"method"`
	Path   string `yaml:"path"`
	Status int    `yaml:"status"`
	Body   string `yaml:"body"`
	Fault  string `yaml:"fault"`
	OK     bool   `yaml:"ok"`
}

// TestKickstartAnswerFaultRefusesWhatTheContractRefuses shows the check that
// holds every kickstart answer to the contract's record can refuse. It runs on
// each answer of each kickstart fixture, and a check that reported nothing
// would leave all of them green, so these answers are the proof: each bad one
// names its fault, and each one the contract allows, or the check is not asked
// to judge, names none.
func TestKickstartAnswerFaultRefusesWhatTheContractRefuses(t *testing.T) {
	t.Parallel()
	contract, reason := loadKickstartContract(t)
	if contract == nil {
		t.Skipf("the kickstart error record is not on this machine: %s; make check reads it from the module cache", reason)
	}
	var fixture struct {
		RequiredNames []string                     `yaml:"requiredNames"`
		Cases         []kickstartContractFaultCase `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(kickstartContractFaultsYAML, &fixture); err != nil {
		t.Fatalf("testdata/kickstart_contract_faults.yaml: %v", err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, c := range fixture.Cases {
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("internal/api/testdata/kickstart_contract_faults.yaml", "case", fixture.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if c.OK == (c.Fault != "") {
				t.Fatalf("case %q must name exactly one of fault and ok", c.Name)
			}
			got := kickstartAnswerFault(contract, c.Method, c.Path, c.Status, []byte(c.Body))
			switch {
			case c.OK && got != "":
				t.Errorf("%s %s answered %d: fault %q, want none", c.Method, c.Path, c.Status, got)
			case !c.OK && !strings.Contains(got, c.Fault):
				t.Errorf("%s %s answered %d: fault %q, want one containing %q", c.Method, c.Path, c.Status, got, c.Fault)
			}
		})
	}
}

// TestKickstartContractRecordIsReadable holds the reader of the schema module's
// record to what it claims to read: every kickstart route the server mounts is
// in it, and the status it records for each code is the one the closed set
// answers with.
func TestKickstartContractRecordIsReadable(t *testing.T) {
	t.Parallel()
	contract, reason := loadKickstartContract(t)
	if contract == nil {
		t.Skipf("the kickstart error record is not on this machine: %s; make check reads it from the module cache", reason)
	}
	mounted := map[string]bool{}
	for _, route := range []struct{ method, path string }{
		{"GET", defaults.RouteKickstartStatus.String()},
		{"POST", defaults.RouteKickstartSessions.String()},
		{"GET", defaults.RouteKickstartSession.String()},
		{"PATCH", defaults.RouteKickstartSession.String()},
		{"GET", defaults.RouteKickstartTree.String()},
		{"GET", defaults.RouteKickstartPreview.String()},
		{"POST", defaults.RouteKickstartScan.String()},
		{"POST", defaults.RouteKickstartCommit.String()},
	} {
		found := contract.route(route.method, strings.NewReplacer("{id}", "x", "{nodeId}", "y").Replace(route.path))
		if found == nil {
			t.Errorf("%s %s is mounted and the contract record has no such route", route.method, route.path)
			continue
		}
		mounted[found.OperationID] = true
	}
	for _, operation := range contract.RequiredRoutes {
		if !mounted[operation] {
			t.Errorf("the contract requires %s and no mounted route matches it", operation)
		}
	}
	var codes []string
	for _, code := range schema.AllLocalKickstartErrorCodes {
		codes = append(codes, code.String())
		if got, ok := contract.CodeStatus[code.String()]; !ok || got != code.HTTPStatus() {
			t.Errorf("code %s answers %d, the record says %d (present %t)", code, code.HTTPStatus(), got, ok)
		}
	}
	sort.Strings(codes)
	var recorded []string
	for code := range contract.CodeStatus {
		recorded = append(recorded, code)
	}
	sort.Strings(recorded)
	if strings.Join(codes, ",") != strings.Join(recorded, ",") {
		t.Errorf("the record lists codes %v, the closed set has %v", recorded, codes)
	}
}

// loadSettingsErrorsContract reads the schema module's record of the failures
// the settings read answers: operation id, then status, then the tokens.
func loadSettingsErrorsContract(t testing.TB) (map[string]map[string][]string, string) {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/peasant-labs/schema").Output()
	dir := strings.TrimSpace(string(out))
	if err != nil || dir == "" {
		return nil, fmt.Sprintf("the schema module's directory is not available (go list: %v)", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "testdata", "local-api", "settings_errors.yaml"))
	if err != nil {
		return nil, fmt.Sprintf("the schema module at %s carries no settings_errors.yaml: %v", dir, err)
	}
	var record struct {
		Routes []struct {
			OperationID string              `yaml:"operation_id"`
			Errors      map[string][]string `yaml:"errors"`
		} `yaml:"routes"`
	}
	if err := yaml.Unmarshal(data, &record); err != nil {
		return nil, fmt.Sprintf("settings_errors.yaml does not decode: %v", err)
	}
	declared := map[string]map[string][]string{}
	for _, route := range record.Routes {
		declared[route.OperationID] = route.Errors
	}
	return declared, ""
}
