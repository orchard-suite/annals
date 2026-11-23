// Package graph is the part of Microsoft Graph Annals calls: a client
// that signs in as an app registration with a federated workload token (no
// secret) and sends JSON requests. The Microsoft 365 collector reads the
// audit log through it, and the people directory reads the tenant's users.
package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Scope is the scope a token for Graph asks for.
const Scope = "https://graph.microsoft.com/.default"

// Client sends requests to Graph.
type Client struct {
	base   string // such as https://graph.microsoft.com/v1.0
	tokens TokenSource
	http   *http.Client
}

// TokenSource answers a bearer token for Graph.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

func New(base string, tokens TokenSource, client *http.Client) *Client {
	return &Client{base: strings.TrimRight(base, "/"), tokens: tokens, http: client}
}

// FederatedToken signs in as an app registration with the workload's
// projected service account token as the client assertion. It keeps the
// access token until shortly before it expires.
type FederatedToken struct {
	LoginURL, TenantID, ClientID, AssertionFile, Scope string
	HTTP                                               *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func (f *FederatedToken) Token(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.token != "" && time.Until(f.expires) > 5*time.Minute {
		return f.token, nil
	}
	assertion, err := os.ReadFile(f.AssertionFile)
	if err != nil {
		return "", fmt.Errorf("read the workload token: %w", err)
	}
	form := url.Values{
		"client_id":             {f.ClientID},
		"scope":                 {f.Scope},
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {strings.TrimSpace(string(assertion))},
	}
	u := strings.TrimRight(f.LoginURL, "/") + "/" + url.PathEscape(f.TenantID) + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := f.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body)
	if res.StatusCode >= 300 || body.AccessToken == "" {
		return "", fmt.Errorf("sign in answered %d: %s %s", res.StatusCode, body.Error, firstLine(body.Description))
	}
	f.token, f.expires = body.AccessToken, time.Now().Add(time.Duration(body.ExpiresIn)*time.Second)
	return f.token, nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return strings.TrimSpace(s)
}

// Do sends one request and decodes its JSON answer into out. path is
// relative to the client's base, or a whole URL Graph handed back, such as
// a next or delta link.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return err
	}
	u := path
	if !strings.HasPrefix(path, "https://") && !strings.HasPrefix(path, "http://") {
		u = c.base + path
	}
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if res.StatusCode >= 300 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		json.Unmarshal(raw, &e)
		return &Error{Status: res.StatusCode, Code: e.Error.Code, Message: firstLine(e.Error.Message)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Error is Graph refusing a request.
type Error struct {
	Status        int
	Code, Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("graph answered %d %s: %s", e.Status, e.Code, e.Message)
}
