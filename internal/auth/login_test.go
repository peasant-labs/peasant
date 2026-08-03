package auth

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// mockExchangeServer creates a test HTTP server that simulates the village
// exchange endpoint. It validates that the correct code+state are posted and
// returns a canned credentials response.
func mockExchangeServer(t *testing.T, expectedCode, expectedState string, resp exchangeCodeResponse) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/cli/exchange" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req exchangeCodeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request body: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.Code != expectedCode {
			t.Errorf("exchange code = %q, want %q", req.Code, expectedCode)
		}
		if req.State != expectedState {
			t.Errorf("exchange state = %q, want %q", req.State, expectedState)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestCallbackServer_Success(t *testing.T) {
	exchangeResp := exchangeCodeResponse{
		APIKey:   "peasant_key123",
		KeyID:    "kid",
		UserID:   "uid",
		Username: "octocat",
	}

	state := "test-state-123"
	code := "exchange-code-abc"

	village := mockExchangeServer(t, code, state, exchangeResp)
	defer village.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	resultCh := make(chan LoginResult, 1)
	srv := newCallbackServer(ln, state, village.URL, resultCh)

	go func() {
		_ = srv.serve()
	}()
	defer srv.shutdown()

	port := ln.Addr().(*net.TCPAddr).Port
	callbackURL := fmt.Sprintf("http://127.0.0.1:%d/callback?code=%s&state=%s",
		port,
		url.QueryEscape(code),
		url.QueryEscape(state),
	)

	resp, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	result := <-resultCh
	if result.Err != nil {
		t.Fatalf("result error: %v", result.Err)
	}
	if result.Credentials.APIKey != "peasant_key123" {
		t.Errorf("APIKey = %q, want %q", result.Credentials.APIKey, "peasant_key123")
	}
	if result.Credentials.Username != "octocat" {
		t.Errorf("Username = %q, want %q", result.Credentials.Username, "octocat")
	}
	if result.Credentials.KeyID != "kid" {
		t.Errorf("KeyID = %q, want %q", result.Credentials.KeyID, "kid")
	}
	if result.Credentials.UserID != "uid" {
		t.Errorf("UserID = %q, want %q", result.Credentials.UserID, "uid")
	}
}

func TestCallbackServer_InvalidState(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	resultCh := make(chan LoginResult, 1)
	// Use a dummy village URL — it should never be called because state
	// validation fails before the exchange.
	srv := newCallbackServer(ln, "correct-state", "http://127.0.0.1:0", resultCh)

	go func() {
		_ = srv.serve()
	}()
	defer srv.shutdown()

	port := ln.Addr().(*net.TCPAddr).Port
	callbackURL := fmt.Sprintf("http://127.0.0.1:%d/callback?code=c&state=wrong-state", port)

	resp, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	result := <-resultCh
	if result.Err == nil {
		t.Fatal("expected error for invalid state")
	}
}

func TestCallbackServer_MissingCode(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	resultCh := make(chan LoginResult, 1)
	state := "test-state"
	// Use a dummy village URL — it should never be called.
	srv := newCallbackServer(ln, state, "http://127.0.0.1:0", resultCh)

	go func() {
		_ = srv.serve()
	}()
	defer srv.shutdown()

	port := ln.Addr().(*net.TCPAddr).Port
	// Missing code param
	callbackURL := fmt.Sprintf("http://127.0.0.1:%d/callback?state=%s", port, state)

	resp, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	result := <-resultCh
	if result.Err == nil {
		t.Fatal("expected error for missing code")
	}
}

func TestStartListener_Loopback(t *testing.T) {
	ln, err := startListener()
	if err != nil {
		t.Fatalf("startListener: %v", err)
	}
	defer ln.Close()

	tcpAddr := ln.Addr().(*net.TCPAddr)
	if tcpAddr.IP.String() != "127.0.0.1" {
		t.Errorf("listener IP = %s, want 127.0.0.1", tcpAddr.IP)
	}
}
