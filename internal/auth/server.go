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
	"github.com/peasant-labs/peasant/internal/htmlpage"
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
		w.Write(renderErrorPage("the login could not be verified", "the login state that came back does not match the one this computer sent."))
		s.resultCh <- LoginResult{Err: fmt.Errorf("state mismatch")}
		return
	}

	code := q.Get("code")
	if code == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		w.Write(renderErrorPage("the login came back incomplete", "peasant online sent this computer no login code."))
		s.resultCh <- LoginResult{Err: fmt.Errorf("missing code parameter")}
		return
	}

	creds, err := exchangeCode(s.villageURL, code, s.state)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		w.Write(renderErrorPage("the login could not finish", "peasant online did not accept the login code from this computer."))
		s.resultCh <- LoginResult{Err: fmt.Errorf("exchange code: %w", err)}
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(renderSuccessPage(creds.Username, s.villageURL))
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

// The pages a browser lands on when the login comes back to this terminal.
// They name the hosted app "peasant online" and leave the account handle in
// its own case; html/template escapes it.
const (
	loginPageTitleOK     = "logged in | peasant"
	loginPageTitleFailed = "login failed | peasant"
	loginPageHintOK      = "you can close this tab and go back to peasant."
	loginPageHintFailed  = "close this tab and log in again from peasant."
)

func renderSuccessPage(username, villageURL string) []byte {
	page := htmlpage.Page{
		Title:   loginPageTitleOK,
		Heading: "welcome to peasant",
		Lines:   []htmlpage.Line{{htmlpage.Text("logged in as "), htmlpage.Value(username)}},
		Status:  &htmlpage.Status{Tone: htmlpage.ToneSuccess, Text: "linked to peasant online"},
		Hint:    loginPageHintOK,
	}
	if u, err := url.Parse(villageURL); err == nil {
		h := u.Hostname()
		if h == "localhost" || h == "127.0.0.1" {
			page.Note = "test server · " + u.Host
		}
	}
	return htmlpage.MustRender(page)
}

func renderErrorPage(heading, detail string) []byte {
	return htmlpage.MustRender(htmlpage.Page{
		Title:   loginPageTitleFailed,
		Heading: heading,
		Lines:   []htmlpage.Line{{htmlpage.Text(detail)}},
		Status:  &htmlpage.Status{Tone: htmlpage.ToneDanger, Text: "not logged in"},
		Hint:    loginPageHintFailed,
	})
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
