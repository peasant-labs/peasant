// Copyright 2026 Peasant Labs
// SPDX-License-Identifier: ISC

package sqlite

import (
	"bytes"
	_ "embed"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/connection-lifetime.yaml
var connectionLifetimeFixture []byte

const authorizationDenialFixtureName = "retired cleanup cannot bypass the current authorization denial"

type connectionLifetimeCase struct {
	Name       string `yaml:"name"`
	Scenario   string `yaml:"scenario"`
	DenySelect bool   `yaml:"deny_select"`
}

func loadConnectionLifetimeFixtures(t *testing.T) []connectionLifetimeCase {
	t.Helper()
	var fixture struct {
		Cases []connectionLifetimeCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(connectionLifetimeFixture))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("unexpected trailing fixture document: %v", err)
	}
	required := map[string]bool{
		"retired cleanup preserves a newly registered connection":            false,
		"retired cleanup preserves current authorization and busy callbacks": false,
		authorizationDenialFixtureName:                                       false,
		"successful close releases its own registrations":                    false,
		"duplicate close cannot clean a replacement connection":              false,
		"nil close retains its explicit error":                               false,
	}
	for _, c := range fixture.Cases {
		seen, ok := required[c.Name]
		if !ok || seen {
			t.Fatalf("unknown or duplicate connection lifetime fixture %q", c.Name)
		}
		required[c.Name] = true
		if c.Name == authorizationDenialFixtureName {
			if !c.DenySelect || c.Scenario != "replacement" {
				t.Fatal("authorization denial fixture must deny SELECT on the replacement connection")
			}
		} else if c.DenySelect {
			t.Fatalf("fixture %q cannot claim authorization denial", c.Name)
		}
		switch c.Scenario {
		case "replacement-before-callbacks", "replacement", "owner-close", "duplicate-close", "nil-close":
		default:
			t.Fatalf("unknown connection lifetime scenario %q", c.Scenario)
		}
	}
	for name, seen := range required {
		if !seen {
			t.Fatalf("missing connection lifetime fixture %q", name)
		}
	}
	return fixture.Cases
}

func TestConnectionRegistrationLifetime(t *testing.T) {
	for _, c := range loadConnectionLifetimeFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			if c.Scenario == "nil-close" {
				var conn *Conn
				if err := conn.Close(); err == nil || !strings.Contains(err.Error(), "nil connection") {
					t.Fatalf("nil Close = %v", err)
				}
				return
			}
			path := filepath.Join(t.TempDir(), "lifetime.sqlite")
			current, err := OpenConn(path, OpenReadWrite|OpenCreate|OpenPrivateCache)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if !current.closed {
					if err := current.Close(); err != nil {
						t.Error(err)
					}
				}
			})
			if c.Scenario == "replacement-before-callbacks" {
				// Model the boundary after OpenConn registers the new generation,
				// before application code installs callbacks.
				retired := &Conn{conn: current.conn, closed: true}
				retired.releaseConnectionRegistrations()
				assertConnectionOwner(t, current)
				executeLifetimeSQL(t, current, "SELECT 1", ResultOK)
				return
			}
			authorizerCalls := 0
			auth := AuthorizeFunc(func(action Action) AuthResult {
				authorizerCalls++
				if c.DenySelect && action.Type() == OpSelect {
					return AuthResultDeny
				}
				return AuthResultOK
			})
			if err := current.SetAuthorizer(auth); err != nil {
				t.Fatal(err)
			}
			busyCalls := 0
			current.setBusyHandler(func(int) bool { busyCalls++; return false })
			if c.Scenario == "owner-close" {
				address := current.conn
				if err := current.Close(); err != nil {
					t.Fatal(err)
				}
				authorizers.mu.RLock()
				_, hasAuth := authorizers.m[address]
				authorizers.mu.RUnlock()
				_, hasBusy := busyHandlers.Load(address)
				allConns.mu.RLock()
				_, hasConn := allConns.table[address]
				allConns.mu.RUnlock()
				if hasAuth || hasBusy || hasConn {
					t.Fatalf("closed owner retained registrations: auth=%t busy=%t connection=%t", hasAuth, hasBusy, hasConn)
				}
				return
			}
			// Model an old connection just past native close at the reused address.
			// SQL below executes on the real current connection, without allocator timing.
			retired := &Conn{conn: current.conn, closed: true}
			if c.Scenario == "duplicate-close" {
				if err := retired.Close(); err == nil || !strings.Contains(err.Error(), "already closed") {
					t.Fatalf("duplicate Close=%v", err)
				}
			} else {
				retired.releaseConnectionRegistrations()
			}
			// Check presence first: unfixed preparation otherwise panics in the trampoline.
			authorizers.mu.RLock()
			_, hasAuth := authorizers.m[current.conn]
			authorizers.mu.RUnlock()
			if !hasAuth {
				t.Fatal("retired cleanup removed the current authorizer")
			}
			assertConnectionOwner(t, current)
			expected := ResultOK
			if c.DenySelect {
				expected = ResultAuth
			}
			executeLifetimeSQL(t, current, "SELECT 1", expected)
			if authorizerCalls == 0 {
				t.Fatal("real preparation did not invoke the current authorizer")
			}
			// Actual SQLite lock contention without a sleep or goroutine.
			writer, err := OpenConn(path, OpenReadWrite|OpenPrivateCache)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := writer.Close(); err != nil {
					t.Error(err)
				}
			}()
			executeLifetimeSQL(t, writer, "CREATE TABLE writes (value INTEGER)", ResultOK)
			executeLifetimeSQL(t, writer, "BEGIN IMMEDIATE", ResultOK)
			executeLifetimeSQL(t, current, "INSERT INTO writes VALUES (1)", ResultBusy)
			executeLifetimeSQL(t, writer, "ROLLBACK", ResultOK)
			if busyCalls == 0 {
				t.Fatal("retired cleanup removed the current busy callback")
			}
		})
	}
}

func assertConnectionOwner(t *testing.T, c *Conn) {
	t.Helper()
	allConns.mu.RLock()
	owner := allConns.table[c.conn]
	allConns.mu.RUnlock()
	if owner != c {
		t.Fatal("retired cleanup removed the current connection registration")
	}
}

func executeLifetimeSQL(t *testing.T, c *Conn, query string, expected ResultCode) {
	t.Helper()
	stmt, _, err := c.PrepareTransient(query)
	if err == nil {
		_, err = stmt.Step()
		if finalizeErr := stmt.Finalize(); err == nil {
			err = finalizeErr
		}
	}
	if got := ErrCode(err); got != expected {
		t.Fatalf("%s: result=%v want=%v, error=%v", query, got, expected, err)
	}
}
