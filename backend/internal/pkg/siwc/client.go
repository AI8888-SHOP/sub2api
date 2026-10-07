// Package siwc implements the OpenClaw 2026.9.8 SIWC wire contract.
// It never uses Codex's client ID, token endpoint or ChatGPT inference backend.
package siwc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/sync/singleflight"
)

const (
	Issuer      = "https://auth.openai.com"
	Resource    = "https://api.openai.com/v1"
	Redirect    = "http://localhost:8080/auth/callback"
	EntryClient = "dynamic_agent_client"
	Scope       = "openid email profile resource.invoke chatgpt.tokens.use.direct offline_access"
	Sharing     = "chatgpt-token-sharing"
	Identity    = "chatgpt-identity"
	UserAgent   = "OpenAI/JS 7.23.0"
)

var registeredClient = regexp.MustCompile(`^oaiapp_[A-Za-z0-9_-]+$`)

type HTTPError struct {
	StatusCode int
	Code       string
}

func (e *HTTPError) Error() string {
	if e.Code == "invalid_grant" {
		return "SIWC invalid_grant; requires re-login"
	}
	if e.Code == "unsupported_country_region_territory" {
		return "SIWC upstream rejected the server region (HTTP 403)"
	}
	return fmt.Sprintf("SIWC upstream returned HTTP %d", e.StatusCode)
}

var ErrInvalid = errors.New("invalid SIWC authorization; start sign-in again")

type Credential struct {
	Access             string `json:"access_token"`
	Refresh            string `json:"refresh_token"`
	IDToken            string `json:"id_token"`
	ClientID           string `json:"client_id"`
	Subject            string `json:"siwc_subject"`
	IdentityKey        string `json:"siwc_identity"`
	Email              string `json:"email"`
	GrantedScope       string `json:"granted_scope"`
	AuthorizationScope string `json:"authorization_scope"`
	AuthFlow           string `json:"auth_flow"`
	ExpiresAt          string `json:"expires_at"`
}

func (c Credential) Map() map[string]any {
	b, _ := json.Marshal(c)
	var result map[string]any
	_ = json.Unmarshal(b, &result)
	result["auth_mode"] = "siwc"
	result["issuer"] = Issuer
	return result
}
func FromMap(m map[string]any) Credential {
	b, _ := json.Marshal(m)
	var c Credential
	_ = json.Unmarshal(b, &c)
	return c
}
func HasSharing(scope string) bool {
	s := map[string]bool{}
	for _, v := range strings.Fields(scope) {
		s[v] = true
	}
	return s["resource.invoke"] && (s["chatgpt.tokens.use.direct"] || s["chatpass.enable.request.direct"])
}

type Session struct {
	State, Nonce, Verifier, ClientID, Proxy, AuthorizationScope string
	Previous                                                    *Credential
	Created                                                     time.Time
}
type Authorization struct {
	AuthURL   string `json:"auth_url"`
	SessionID string `json:"session_id"`
}
type refreshReplay struct {
	credential Credential
	created    time.Time
}

type Client struct {
	refreshMu    sync.Mutex
	refreshGroup singleflight.Group
	refreshed    map[string]refreshReplay
	mu           sync.Mutex
	sessions     map[string]Session
	// Injectable only in package tests; production endpoints are fixed.
	do func(context.Context, string, *http.Request) (*http.Response, error)
}

func New() *Client { return &Client{sessions: map[string]Session{}, do: doRequest} }
func random() (string, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b), err
}
func (c *Client) Start(proxy string, previous *Credential) (*Authorization, error) {
	vals := make([]string, 4)
	for i := range vals {
		v, e := random()
		if e != nil {
			return nil, e
		}
		vals[i] = v
	}
	s := Session{State: vals[0], Nonce: vals[1], Verifier: vals[2], ClientID: EntryClient, Proxy: proxy, AuthorizationScope: Scope, Created: time.Now()}
	if previous != nil {
		if !registeredClient.MatchString(previous.ClientID) || previous.Subject == "" || previous.IdentityKey == "" {
			return nil, ErrInvalid
		}
		p := *previous
		s.Previous = &p
		s.ClientID = p.ClientID
		if p.AuthorizationScope != "" {
			s.AuthorizationScope = p.AuthorizationScope
		}
	}
	digest := sha256.Sum256([]byte(s.Verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {s.ClientID}, "redirect_uri": {Redirect}, "resource": {Resource}, "scope": {s.AuthorizationScope}, "state": {s.State}, "nonce": {s.Nonce}, "code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])}, "code_challenge_method": {"S256"}}
	if previous == nil {
		q.Set("agent_name_hint", "OpenClaw")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, v := range c.sessions {
		if time.Since(v.Created) > 30*time.Minute {
			delete(c.sessions, id)
		}
	}
	if len(c.sessions) >= 64 {
		return nil, errors.New("too many pending SIWC logins")
	}
	c.sessions[vals[3]] = s
	return &Authorization{AuthURL: Issuer + "/api/accounts/authorize?" + q.Encode(), SessionID: vals[3]}, nil
}
func (c *Client) Cancel(id string) { c.mu.Lock(); defer c.mu.Unlock(); delete(c.sessions, id) }
func (c *Client) Exchange(ctx context.Context, id, callback string) (*Credential, error) {
	u, e := url.Parse(callback)
	if e != nil || len(callback) > 16384 || u.Scheme != "http" || u.Host != "localhost:8080" || u.Path != "/auth/callback" || u.RawPath != "" || u.Fragment != "" || u.User != nil {
		return nil, ErrInvalid
	}
	q, e := url.ParseQuery(u.RawQuery)
	if e != nil || len(q["code"]) != 1 || q.Get("code") == "" || len(q["state"]) != 1 || q.Has("error") {
		return nil, ErrInvalid
	}
	c.mu.Lock()
	s, ok := c.sessions[id]
	if !ok || time.Since(s.Created) > 30*time.Minute || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(s.State)) != 1 {
		c.mu.Unlock()
		return nil, ErrInvalid
	}
	clientID := s.ClientID
	if clientID == EntryClient {
		if len(q["client_id"]) != 1 || !registeredClient.MatchString(q.Get("client_id")) {
			c.mu.Unlock()
			return nil, ErrInvalid
		}
		clientID = q.Get("client_id")
	} else if q.Has("client_id") && (len(q["client_id"]) != 1 || q.Get("client_id") != clientID) {
		c.mu.Unlock()
		return nil, ErrInvalid
	}
	delete(c.sessions, id)
	c.mu.Unlock() // Single consumer, even if exchange fails.
	form := url.Values{"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "code_verifier": {s.Verifier}, "client_id": {clientID}, "redirect_uri": {Redirect}, "resource": {Resource}}
	return c.token(ctx, s.Proxy, form, clientID, s.Nonce, s.AuthorizationScope, s.Previous)
}
func (c *Client) Refresh(ctx context.Context, previous Credential, proxy string) (*Credential, error) {
	if !registeredClient.MatchString(previous.ClientID) || previous.Refresh == "" || previous.Subject == "" || previous.IdentityKey != identityKey(previous.ClientID, previous.Subject) {
		return nil, ErrInvalid
	}
	digest := sha256.Sum256([]byte(previous.IdentityKey + "\x00" + previous.Refresh))
	replayKey := hex.EncodeToString(digest[:])
	// Deduplicate rotated-token refreshes without blocking other identities.
	result, err, _ := c.refreshGroup.Do(replayKey, func() (any, error) {
		return c.refresh(ctx, previous, proxy, replayKey)
	})
	if err != nil {
		return nil, err
	}
	out := *result.(*Credential)
	return &out, nil
}
func (c *Client) refresh(ctx context.Context, previous Credential, proxy, replayKey string) (*Credential, error) {
	c.refreshMu.Lock()
	if replay, ok := c.refreshed[replayKey]; ok && time.Since(replay.created) < 5*time.Minute {
		out := replay.credential
		c.refreshMu.Unlock()
		return &out, nil
	}
	c.refreshMu.Unlock()
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {previous.ClientID}, "refresh_token": {previous.Refresh}, "resource": {Resource}}
	result, err := c.token(ctx, proxy, form, previous.ClientID, "", previous.AuthorizationScope, &previous)
	if err != nil {
		return nil, err
	}
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if c.refreshed == nil {
		c.refreshed = map[string]refreshReplay{}
	}
	for key, replay := range c.refreshed {
		if time.Since(replay.created) >= 5*time.Minute {
			delete(c.refreshed, key)
		}
	}
	if len(c.refreshed) >= 64 {
		for key := range c.refreshed {
			delete(c.refreshed, key)
			break
		}
	}
	c.refreshed[replayKey] = refreshReplay{credential: *result, created: time.Now()}
	return result, nil
}
func doRequest(ctx context.Context, proxy string, req *http.Request) (*http.Response, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	if proxy != "" {
		u, e := url.Parse(proxy)
		if e != nil {
			return nil, errors.New("invalid SIWC proxy")
		}
		tr.Proxy = http.ProxyURL(u)
	}
	defer tr.CloseIdleConnections()
	client := http.Client{Transport: tr, Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return client.Do(req.WithContext(ctx))
}
func (c *Client) read(ctx context.Context, proxy string, req *http.Request, target any) error {
	req.Header.Set("User-Agent", "node") // Node fetch default; OpenClaw adds no OAuth/catalog UA.
	resp, e := c.do(ctx, proxy, req)
	if e != nil {
		return errors.New("SIWC upstream connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var envelope struct {
			Error json.RawMessage `json:"error"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
		_ = json.Unmarshal(raw, &envelope)
		code := ""
		_ = json.Unmarshal(envelope.Error, &code)
		if code == "" {
			var detail struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(envelope.Error, &detail)
			code = detail.Code
		}
		if code != "invalid_grant" && code != "unsupported_country_region_territory" {
			code = ""
		}
		return &HTTPError{StatusCode: resp.StatusCode, Code: code}
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if e != nil {
		return errors.New("SIWC upstream read failed")
	}
	if json.Unmarshal(b, target) != nil {
		return errors.New("invalid SIWC upstream response")
	}
	return nil
}
func identityKey(clientID, sub string) string {
	d := sha256.Sum256([]byte(Issuer + "\x00" + clientID + "\x00" + sub))
	return hex.EncodeToString(d[:])
}
func (c *Client) verify(ctx context.Context, proxy, raw, clientID, nonce string) (jwt.MapClaims, error) {
	req, _ := http.NewRequest("GET", Issuer+"/.well-known/jwks.json", nil)
	var keys struct {
		Keys []struct{ Kty, Use, Alg, Kid, N, E string }
	}
	if e := c.read(ctx, proxy, req, &keys); e != nil {
		return nil, e
	}
	claims := jwt.MapClaims{}
	_, e := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, ok := t.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, ErrInvalid
		}
		var key *rsa.PublicKey
		for _, k := range keys.Keys {
			if k.Kid != kid || k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
				continue
			}
			n, en := base64.RawURLEncoding.DecodeString(k.N)
			eb, ee := base64.RawURLEncoding.DecodeString(k.E)
			if en != nil || ee != nil || len(n) < 256 || len(eb) == 0 || len(eb) > 4 || key != nil {
				return nil, ErrInvalid
			}
			exp := 0
			for _, v := range eb {
				exp = exp*256 + int(v)
			}
			if exp < 3 || exp%2 == 0 {
				return nil, ErrInvalid
			}
			key = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
		}
		if key == nil {
			return nil, ErrInvalid
		}
		return key, nil
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(Issuer), jwt.WithAudience(clientID), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if e != nil {
		return nil, ErrInvalid
	}
	sub, _ := claims.GetSubject()
	aud, _ := claims.GetAudience()
	iat, ie := claims.GetIssuedAt()
	if sub == "" || ie != nil || iat == nil || (nonce != "" && claims["nonce"] != nonce) || (claims["azp"] != nil && claims["azp"] != clientID) || (len(aud) > 1 && claims["azp"] != clientID) {
		return nil, ErrInvalid
	}
	return claims, nil
}
func (c *Client) token(ctx context.Context, proxy string, form url.Values, clientID, nonce, requested string, previous *Credential) (*Credential, error) {
	req, _ := http.NewRequest("POST", Issuer+"/api/accounts/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var t struct {
		Access  string  `json:"access_token"`
		Refresh string  `json:"refresh_token"`
		ID      string  `json:"id_token"`
		Type    string  `json:"token_type"`
		Scope   *string `json:"scope"`
		Expires int64   `json:"expires_in"`
	}
	if e := c.read(ctx, proxy, req, &t); e != nil {
		return nil, e
	}
	out := Credential{}
	if previous != nil {
		out = *previous
	}
	if t.Access == "" || !strings.EqualFold(t.Type, "Bearer") || t.Expires <= 0 || t.Expires > 31536000 {
		return nil, ErrInvalid
	}
	out.Access = t.Access
	out.ClientID = clientID
	out.AuthorizationScope = requested
	out.ExpiresAt = time.Now().Add(time.Duration(t.Expires) * time.Second).UTC().Format(time.RFC3339)
	if t.Refresh != "" {
		out.Refresh = t.Refresh
	}
	if t.Scope != nil {
		out.GrantedScope = *t.Scope
	} else if previous == nil {
		return nil, ErrInvalid
	}
	if t.ID != "" {
		claims, e := c.verify(ctx, proxy, t.ID, clientID, nonce)
		if e != nil {
			return nil, e
		}
		out.IDToken = t.ID
		out.Subject, _ = claims.GetSubject()
		out.Email, _ = claims["email"].(string)
	} else if previous == nil || nonce != "" {
		return nil, ErrInvalid
	}
	if out.Refresh == "" || out.IDToken == "" || out.Subject == "" {
		return nil, ErrInvalid
	}
	out.IdentityKey = identityKey(clientID, out.Subject)
	if previous != nil && (out.IdentityKey != previous.IdentityKey || out.Subject != previous.Subject) {
		return nil, errors.New("SIWC account changed; reconnect the original identity")
	}
	out.AuthFlow = Identity
	if HasSharing(out.GrantedScope) {
		out.AuthFlow = Sharing
	}
	return &out, nil
}

// NormalizePayload is deliberately applied last, including on retries.
func NormalizePayload(body []byte) ([]byte, error) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if dec.Decode(&m) != nil || m == nil {
		return nil, errors.New("invalid SIWC Responses body")
	}
	var tail any
	if dec.Decode(&tail) != io.EOF {
		return nil, errors.New("invalid SIWC Responses body")
	}
	for _, k := range []string{"previous_response_id", "conversation", "background"} {
		if v, ok := m[k]; ok && v != nil && v != false && v != "" {
			return nil, fmt.Errorf("SIWC does not support %s; replay full context", k)
		}
	}
	if tier, ok := m["service_tier"]; ok {
		switch tier {
		case "default", "priority", "ultrafast", "slow":
		default:
			return nil, errors.New("unsupported SIWC service_tier")
		}
	}
	if m["audio"] != nil {
		return nil, errors.New("SIWC does not support audio endpoints")
	}
	if modalities, ok := m["modalities"].([]any); ok {
		for _, v := range modalities {
			if v != "text" {
				return nil, errors.New("SIWC supports text output only")
			}
		}
	}
	if tools, ok := m["tools"].([]any); ok {
		for _, tool := range tools {
			v, ok := tool.(map[string]any)
			if !ok {
				return nil, ErrInvalid
			}
			switch v["type"] {
			case "function", "web_search", "web_search_preview":
			default:
				return nil, errors.New("SIWC supports only function and web_search tools")
			}
		}
	}
	if input, ok := m["input"].([]any); ok {
		for _, item := range input {
			v, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("invalid SIWC input item")
			}
			if v["type"] == "item_reference" {
				return nil, errors.New("SIWC requires full input items")
			}
			if parts, ok := v["content"].([]any); ok {
				for _, part := range parts {
					p, ok := part.(map[string]any)
					if !ok {
						return nil, errors.New("invalid SIWC input content")
					}
					switch p["type"] {
					case "input_audio", "audio":
						return nil, errors.New("SIWC does not support audio input")
					case "item_reference":
						return nil, errors.New("SIWC requires full input content")
					case "input_file":
						if id, exists := p["file_id"]; exists && id != nil && id != "" {
							return nil, errors.New("SIWC requires inline file content")
						}
					}
				}
			}
		}
	}
	for _, k := range []string{"context_management", "metadata", "max_output_tokens", "temperature", "top_p", "prompt_cache_retention", "previous_response_id", "conversation", "background"} {
		delete(m, k)
	}
	m["store"] = false
	m["stream"] = true
	return json.Marshal(m)
}
func InferenceRequest(ctx context.Context, body []byte, token string) (*http.Request, error) {
	body, e := NormalizePayload(body)
	if e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", Resource+"/responses", bytes.NewReader(body))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("x-openai-chatpass-test", "codex-direct")
	req.Header.Set("X-Stainless-Lang", "js")
	req.Header.Set("X-Stainless-Package-Version", "7.23.0")
	req.Header.Set("X-Stainless-Retry-Count", "0")
	return req, nil
}
func (c *Client) Catalog(ctx context.Context, proxy, token string) ([]byte, error) {
	req, _ := http.NewRequest("GET", Resource+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	var catalog map[string]json.RawMessage
	if e := c.read(ctx, proxy, req, &catalog); e != nil {
		return nil, e
	}
	raw := bytes.TrimSpace(catalog["models"])
	if len(raw) == 0 || raw[0] != '[' {
		return nil, errors.New("invalid SIWC model catalog")
	}
	var entries []struct {
		Slug       string `json:"slug"`
		Display    string `json:"display_name"`
		Visibility string `json:"visibility"`
	}
	if json.Unmarshal(raw, &entries) != nil {
		return nil, errors.New("invalid SIWC model catalog")
	}
	models := make([]map[string]any, 0)
	for _, m := range entries {
		if m.Visibility != "list" {
			continue
		}
		if strings.TrimSpace(m.Slug) == "" {
			return nil, errors.New("invalid SIWC model slug")
		}
		models = append(models, map[string]any{"id": m.Slug, "display_name": m.Display, "object": "model", "owned_by": "openai", "created": 0})
	}
	return json.Marshal(map[string]any{"object": "list", "data": models})
}
