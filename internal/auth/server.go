package auth

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// LoginResult holds the outcome of a login callback.
type LoginResult struct {
	Credentials *Credentials
	Err         error
}

// callbackServer is a local HTTP server that receives the OAuth callback.
type callbackServer struct {
	httpSrv    *http.Server
	listener   net.Listener
	state      string
	villageURL string
	resultCh   chan<- LoginResult
}

// newCallbackServer creates a callback server that listens for the OAuth redirect.
func newCallbackServer(listener net.Listener, state string, villageURL string, resultCh chan<- LoginResult) *callbackServer {
	s := &callbackServer{
		listener:   listener,
		state:      state,
		villageURL: villageURL,
		resultCh:   resultCh,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", s.handleCallback)

	s.httpSrv = &http.Server{Handler: mux}
	return s
}

// serve starts accepting connections. Blocks until the server is shut down.
func (s *callbackServer) serve() error {
	err := s.httpSrv.Serve(s.listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// shutdown gracefully shuts down the server.
func (s *callbackServer) shutdown() {
	_ = s.httpSrv.Shutdown(context.Background())
}

// handleCallback receives the OAuth redirect from the village with code+state,
// then calls the exchange endpoint to obtain credentials.
func (s *callbackServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	if q.Get("state") != s.state {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, renderErrorPage("Invalid state parameter", "The authentication state could not be verified. Please try logging in again."))
		s.resultCh <- LoginResult{Err: fmt.Errorf("state mismatch")}
		return
	}

	code := q.Get("code")
	if code == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, renderErrorPage("Missing parameters", "The server response was incomplete. Please try logging in again."))
		s.resultCh <- LoginResult{Err: fmt.Errorf("missing code parameter")}
		return
	}

	creds, err := exchangeCode(s.villageURL, code, s.state)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, renderErrorPage("Exchange failed", "Failed to exchange the authorization code. Please try logging in again."))
		s.resultCh <- LoginResult{Err: fmt.Errorf("exchange code: %w", err)}
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, renderSuccessPage(creds.Username, s.villageURL))
	s.resultCh <- LoginResult{Credentials: creds}
}

// exchangeCodeRequest is the JSON body sent to the village exchange endpoint.
type exchangeCodeRequest struct {
	Code  string `json:"code"`
	State string `json:"state"`
}

// exchangeCodeResponse is the JSON response from the village exchange endpoint.
type exchangeCodeResponse struct {
	APIKey   string `json:"api_key"`
	KeyID    string `json:"key_id"`
	UserID   string `json:"user_id"`
	Username string `json:"username"`
}

// exchangeCode posts {code, state} to the village exchange endpoint and
// returns Credentials on success.
func exchangeCode(villageURL, code, state string) (*Credentials, error) {
	reqBody, err := json.Marshal(exchangeCodeRequest{Code: code, State: state})
	if err != nil {
		return nil, fmt.Errorf("marshal exchange request: %w", err)
	}

	client := &http.Client{
		Timeout: defaults.LoginHTTPTimeout,
	}

	// For local development, allow insecure TLS if the village is on localhost.
	// This helps with self-signed certificates in dev environments.
	if u, err := url.Parse(villageURL); err == nil {
		if u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" {
			client.Transport = &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			}
		}
	}

	resp, err := client.Post(
		villageURL+"/api/v1/auth/cli/exchange",
		"application/json",
		bytes.NewReader(reqBody),
	)
	if err != nil {
		return nil, fmt.Errorf("post to exchange endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exchange endpoint returned status %d", resp.StatusCode)
	}

	var exchResp exchangeCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&exchResp); err != nil {
		return nil, fmt.Errorf("decode exchange response: %w", err)
	}

	if exchResp.APIKey == "" || exchResp.KeyID == "" || exchResp.UserID == "" || exchResp.Username == "" {
		return nil, fmt.Errorf("exchange response missing required fields")
	}

	return &Credentials{
		APIKey:   exchResp.APIKey,
		KeyID:    exchResp.KeyID,
		UserID:   exchResp.UserID,
		Username: exchResp.Username,
	}, nil
}

const pageFontLink = `<link rel="preconnect" href="https://fonts.googleapis.com"><link rel="preconnect" href="https://fonts.gstatic.com" crossorigin><link href="https://fonts.googleapis.com/css2?family=Instrument+Serif:ital@0;1&family=DM+Sans:opsz,wght@9..40,300;9..40,400;9..40,500;9..40,600&family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">`

const pageStyles = `
* { margin: 0; padding: 0; box-sizing: border-box; }
body {
  font-family: "DM Sans", system-ui, sans-serif;
  color: #2a2521;
  background-color: #faf9f7;
  background-image:
    linear-gradient(rgba(0,51,48,0.035) 1px, transparent 1px),
    linear-gradient(90deg, rgba(0,51,48,0.035) 1px, transparent 1px);
  background-size: 48px 48px;
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  -webkit-font-smoothing: antialiased;
}
::selection { background: #90ffe6; color: #003330; }
.card {
  background: white;
  border: 1px solid #e8e4dd;
  border-radius: 12px;
  padding: 52px 56px;
  max-width: 480px;
  width: 100%;
  margin: 24px;
  text-align: center;
  box-shadow: 0 1px 3px rgba(0,0,0,0.04), 0 8px 24px rgba(4,200,165,0.06);
  animation: fade-up 0.5s ease-out both;
}
.logo {
  position: relative;
  width: 48px;
  height: 48px;
  margin: 0 auto 28px;
}
.logo-circle {
  position: absolute;
  border-radius: 50%;
}
.logo-c1 { width: 28px; height: 28px; top: 0; left: 0; background: rgba(29,228,190,0.6); }
.logo-c2 { width: 24px; height: 24px; top: 10px; left: 10px; background: rgba(0,162,136,0.5); }
.logo-c3 { width: 22px; height: 22px; top: 4px; left: 20px; background: rgba(80,245,212,0.45); }
.icon {
  width: 56px;
  height: 56px;
  border-radius: 12px;
  display: flex;
  align-items: center;
  justify-content: center;
  margin: 0 auto 24px;
}
.icon svg { width: 28px; height: 28px; }
.icon-success { background: #effefa; border: 1px solid #c7fff2; color: #00a288; }
.icon-error { background: #fff5f3; border: 1px solid #fb7f6a; color: #df3f27; }
h1 {
  font-family: "Instrument Serif", Georgia, serif;
  font-size: 1.875rem;
  line-height: 1.25;
  font-weight: normal;
  font-style: normal;
  color: #2a2521;
  margin-bottom: 10px;
}
.username {
  color: #00a288;
  font-weight: 500;
}
.subtitle {
  color: #857a6b;
  font-size: 15px;
  line-height: 1.5;
  margin-bottom: 24px;
}
.badge {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  background: #effefa;
  color: #00816e;
  font-family: "JetBrains Mono", "Fira Code", monospace;
  font-size: 11px;
  font-weight: 500;
  text-transform: uppercase;
  letter-spacing: 0.08em;
  padding: 5px 12px;
  border-radius: 6px;
  border: 1px solid #c7fff2;
}
.badge-error {
  background: #fff5f3;
  color: #df3f27;
  border-color: #fb7f6a;
}
.badge-test {
  background: #fffbeb;
  color: #92400e;
  border-color: #fcd34d;
}
.badge-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: currentColor;
  animation: pulse-dot 2s ease-in-out infinite;
}
@keyframes pulse-dot {
  0%, 100% { opacity: 1; }
  50% { opacity: 0.3; }
}
.hint {
  color: #b8b0a1;
  font-family: "JetBrains Mono", "Fira Code", monospace;
  font-size: 11px;
  text-transform: uppercase;
  letter-spacing: 0.1em;
  margin-top: 24px;
}
.brand {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  margin-top: 36px;
  animation: fade-up 0.5s ease-out 0.15s both;
}
.brand-text {
  color: #d5cfc4;
  font-size: 16px;
  font-family: "Instrument Serif", Georgia, serif;
  letter-spacing: 0.5px;
}
.brand-logo {
  position: relative;
  width: 22px;
  height: 22px;
}
.brand-logo .logo-circle { opacity: 0.4; }
.brand-logo .logo-c1 { width: 13px; height: 13px; top: 0; left: 0; }
.brand-logo .logo-c2 { width: 11px; height: 11px; top: 5px; left: 5px; }
.brand-logo .logo-c3 { width: 10px; height: 10px; top: 2px; left: 10px; }
@keyframes fade-up {
  from { opacity: 0; transform: translateY(12px); }
  to { opacity: 1; transform: translateY(0); }
}
`

func renderSuccessPage(username, villageURL string) string {
	badge := `<span class="badge"><span class="badge-dot"></span>CLI linked to village</span>`
	if u, err := url.Parse(villageURL); err == nil {
		h := u.Hostname()
		if h == "localhost" || h == "127.0.0.1" {
			badge = fmt.Sprintf(`<span class="badge badge-test"><span class="badge-dot"></span>Test env &middot; %s</span>`, u.Host)
		}
	}
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Logged in — Peasant</title>
%s<style>%s</style>
</head><body>
<div class="card">
  <div class="icon icon-success">
    <svg fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
      <path d="M9 12.75L11.25 15 15 9.75m-3-7.036A11.959 11.959 0 013.598 6 11.99 11.99 0 003 9.749c0 5.592 3.824 10.29 9 11.623 5.176-1.332 9-6.03 9-11.622 0-1.31-.21-2.571-.598-3.751h-.152c-3.196 0-6.1-1.248-8.25-3.285z"/>
    </svg>
  </div>
  <h1>Welcome to Peasant</h1>
  <p class="subtitle">Logged in as <span class="username">%s</span></p>
  %s
  <p class="hint">You can close this tab and return to your terminal</p>
  <div class="brand">
    <div class="brand-logo">
      <div class="logo-circle logo-c1"></div>
      <div class="logo-circle logo-c2"></div>
      <div class="logo-circle logo-c3"></div>
    </div>
    <span class="brand-text">peasant</span>
  </div>
</div>
</body></html>`, pageFontLink, pageStyles, username, badge)
}

func renderErrorPage(title, detail string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Login failed — Peasant</title>
%s<style>%s</style>
</head><body>
<div class="card">
  <div class="icon icon-error">
    <svg fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
      <path d="M12 9v3.75m-9.303 3.376c-.866 1.5.217 3.374 1.948 3.374h14.71c1.73 0 2.813-1.874 1.948-3.374L13.949 3.378c-.866-1.5-3.032-1.5-3.898 0L2.697 16.126zM12 15.75h.007v.008H12v-.008z"/>
    </svg>
  </div>
  <h1>%s</h1>
  <p class="subtitle">%s</p>
  <span class="badge badge-error"><span class="badge-dot"></span>Authentication failed</span>
  <p class="hint">Close this tab and try again from your terminal</p>
  <div class="brand">
    <div class="brand-logo">
      <div class="logo-circle logo-c1"></div>
      <div class="logo-circle logo-c2"></div>
      <div class="logo-circle logo-c3"></div>
    </div>
    <span class="brand-text">peasant</span>
  </div>
</div>
</body></html>`, pageFontLink, pageStyles, title, detail)
}

// startListener creates a TCP listener on the loopback interface.
// It tries the preferred port first, then falls back to an ephemeral port.
func startListener() (net.Listener, error) {
	addr := "127.0.0.1:" + strconv.Itoa(defaults.LoginCallbackPort)
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln, nil
	}

	// Fall back to ephemeral port.
	ln, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start listener: %w", err)
	}
	return ln, nil
}
