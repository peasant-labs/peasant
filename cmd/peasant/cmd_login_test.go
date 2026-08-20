package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestLoginURLPrinterPrintsExactURLOnStart proves the callback BuildLoginCommand
// wires as auth.LoginFrom's onURL argument prints the exact reported login URL
// immediately, on every call — not conditionally, and not only on a
// browser-open failure. It exercises the real function BuildLoginCommand uses
// (loginURLPrinter), rather than the full RunE, so the assertion does not
// depend on the network/browser side effects a real login triggers (starting a
// local callback listener, invoking the OS browser launcher).
func TestLoginURLPrinterPrintsExactURLOnStart(t *testing.T) {
	var out bytes.Buffer
	const wantURL = "https://village.example.test/api/v1/auth/cli/login?port=54321&state=deadbeef"

	printer := loginURLPrinter(&out)
	printer(wantURL)

	got := out.String()
	if !strings.Contains(got, wantURL) {
		t.Fatalf("printed output = %q, want it to contain the exact login URL %q", got, wantURL)
	}
	if !strings.Contains(strings.ToLower(got), "log in") {
		t.Fatalf("printed output = %q, want a prominent log-in prompt alongside the URL", got)
	}
}

// TestLoginURLPrinterPrintsOnEveryCall proves the printer is unconditional: it
// is not a one-shot or failure-only guard, matching the acceptance criterion
// that the standalone `peasant login` prints the URL on start every run.
func TestLoginURLPrinterPrintsOnEveryCall(t *testing.T) {
	var out bytes.Buffer
	printer := loginURLPrinter(&out)

	printer("https://village.example.test/api/v1/auth/cli/login?port=1&state=a")
	printer("https://village.example.test/api/v1/auth/cli/login?port=2&state=b")

	got := out.String()
	if strings.Count(got, "port=1") != 1 || strings.Count(got, "port=2") != 1 {
		t.Fatalf("printed output = %q, want exactly one line per call", got)
	}
}
