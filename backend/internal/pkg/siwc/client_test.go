package siwc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

func jsonResponse(t *testing.T, value any) *http.Response {
	t.Helper()
	b, e := json.Marshal(value)
	require.NoError(t, e)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b))), Header: make(http.Header)}
}
func TestAuthorizationExchangeRefreshAndIsolation(t *testing.T) {
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, e)
	c := New()
	auth, e := c.Start("", nil)
	require.NoError(t, e)
	u, e := url.Parse(auth.AuthURL)
	require.NoError(t, e)
	require.Equal(t, Issuer+"/api/accounts/authorize", u.Scheme+"://"+u.Host+u.Path)
	require.Equal(t, "OpenClaw", u.Query().Get("agent_name_hint"))
	require.Equal(t, Scope, u.Query().Get("scope"))
	require.NotEqual(t, u.Query().Get("state"), u.Query().Get("nonce"))
	claims := jwt.MapClaims{"iss": Issuer, "aud": "oaiapp_test", "sub": "subject-a", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "nonce": u.Query().Get("nonce"), "email": "a@example.test"}
	sign := func() string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = "test"
		raw, e := tok.SignedString(key)
		require.NoError(t, e)
		return raw
	}
	var calls atomic.Int32
	refresh := false
	changed := false
	scope := Scope
	c.do = func(ctx context.Context, proxy string, r *http.Request) (*http.Response, error) {
		require.Equal(t, "node", r.Header.Get("User-Agent"))
		require.Empty(t, r.Header.Get("chatgpt-account-id"))
		if r.URL.Path == "/.well-known/jwks.json" {
			return jsonResponse(t, map[string]any{"keys": []any{map[string]any{"kty": "RSA", "alg": "RS256", "kid": "test", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}}), nil
		}
		require.Equal(t, "/api/accounts/oauth/token", r.URL.Path)
		require.NoError(t, r.ParseForm())
		require.Equal(t, "oaiapp_test", r.Form.Get("client_id"))
		require.Equal(t, Resource, r.Form.Get("resource"))
		calls.Add(1)
		if refresh {
			require.Equal(t, "refresh_token", r.Form.Get("grant_type"))
			require.Equal(t, "rotate-1", r.Form.Get("refresh_token"))
			if changed {
				claims["sub"] = "different"
				return jsonResponse(t, map[string]any{"access_token": "access-new", "token_type": "Bearer", "expires_in": 3600, "id_token": sign()}), nil
			}
			return jsonResponse(t, map[string]any{"access_token": "access-new", "token_type": "Bearer", "expires_in": 3600}), nil
		}
		require.Equal(t, "authorization_code", r.Form.Get("grant_type"))
		require.NotEmpty(t, r.Form.Get("code_verifier"))
		return jsonResponse(t, map[string]any{"access_token": "access", "refresh_token": "rotate-1", "id_token": sign(), "scope": scope, "token_type": "Bearer", "expires_in": 3600}), nil
	}
	callback := Redirect + "?" + url.Values{"code": {"code"}, "state": {u.Query().Get("state")}, "client_id": {"oaiapp_test"}}.Encode()
	_, e = c.Exchange(context.Background(), auth.SessionID, callback+"&client_id=oaiapp_evil")
	require.Error(t, e)
	require.EqualValues(t, 0, calls.Load())
	cred, e := c.Exchange(context.Background(), auth.SessionID, callback)
	require.NoError(t, e)
	require.Equal(t, Sharing, cred.AuthFlow)
	require.Equal(t, identityKey("oaiapp_test", "subject-a"), cred.IdentityKey)
	_, e = c.Exchange(context.Background(), auth.SessionID, callback)
	require.Error(t, e)
	require.EqualValues(t, 1, calls.Load())
	refresh = true
	updated, e := c.Refresh(context.Background(), *cred, "")
	require.NoError(t, e)
	require.Equal(t, cred.Refresh, updated.Refresh)
	require.Equal(t, cred.IDToken, updated.IDToken)
	require.Equal(t, cred.GrantedScope, updated.GrantedScope)
	c.refreshed = nil
	changed = true
	_, e = c.Refresh(context.Background(), *cred, "")
	require.ErrorContains(t, e, "account changed")
	reconnect, e := c.Start("", cred)
	require.NoError(t, e)
	ru, _ := url.Parse(reconnect.AuthURL)
	require.Equal(t, "oaiapp_test", ru.Query().Get("client_id"))
	require.Empty(t, ru.Query().Get("agent_name_hint"))
}
func TestVerifyRejectsWrongNonceAudienceAlgorithmAndSignature(t *testing.T) {
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, e)
	c := New()
	c.do = func(context.Context, string, *http.Request) (*http.Response, error) {
		return jsonResponse(t, map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "k", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}}), nil
	}
	for _, tc := range []struct {
		name   string
		change func(jwt.MapClaims)
	}{
		{"nonce", func(c jwt.MapClaims) { c["nonce"] = "bad" }}, {"aud", func(c jwt.MapClaims) { c["aud"] = "other" }}, {"issuer", func(c jwt.MapClaims) { c["iss"] = "https://evil.example" }},
		{"missing iat", func(c jwt.MapClaims) { delete(c, "iat") }}, {"multi audience", func(c jwt.MapClaims) { c["aud"] = []string{"oaiapp_test", "other"} }}, {"expired", func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := jwt.MapClaims{"iss": Issuer, "aud": "oaiapp_test", "sub": "s", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "nonce": "expected"}
			tc.change(claims)
			tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
			tok.Header["kid"] = "k"
			raw, e := tok.SignedString(key)
			require.NoError(t, e)
			_, e = c.verify(context.Background(), "", raw, "oaiapp_test", "expected")
			require.Error(t, e)
		})
	}
}
func TestPayloadHeadersAndUnsupportedCapabilities(t *testing.T) {
	req, e := InferenceRequest(context.Background(), []byte(`{"model":"m","input":"hello","store":true,"stream":false,"temperature":1,"max_output_tokens":500,"metadata":{"a":1}}`), "token")
	require.NoError(t, e)
	require.Equal(t, Resource+"/responses", req.URL.String())
	require.Equal(t, UserAgent, req.Header.Get("User-Agent"))
	require.Equal(t, "codex-direct", req.Header.Get("x-openai-chatpass-test"))
	require.Empty(t, req.Header.Get("chatgpt-account-id"))
	require.Empty(t, req.Header.Get("OpenAI-Organization"))
	var body map[string]any
	require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
	require.Equal(t, true, body["stream"])
	require.Equal(t, false, body["store"])
	require.NotContains(t, body, "metadata")
	require.NotContains(t, body, "temperature")
	require.NotContains(t, body, "max_output_tokens")
	for _, raw := range []string{`{"service_tier":"auto"}`, `{"service_tier":"flex"}`, `{"tools":[{"type":"mcp"}]}`, `{"tools":[{"type":"image_generation"}]}`, `{"previous_response_id":"resp_a"}`, `{"input":[{"type":"item_reference","id":"a"}]}`, `{} {}`} {
		_, e := NormalizePayload([]byte(raw))
		require.Error(t, e, raw)
	}
	require.False(t, HasSharing("openid resource.invoke"))
	require.True(t, HasSharing("resource.invoke chatpass.enable.request.direct"))
}
func TestCatalogFiltersAndKeepsAuthoritativeEmpty(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		count int
		valid bool
	}{{`{"models":[{"slug":"listed","visibility":"list"},{"slug":"hidden","visibility":"hidden"}]}`, 1, true}, {`{"models":[]}`, 0, true}, {`{"data":[]}`, 0, false}, {`{"models":null}`, 0, false}} {
		c := New()
		c.do = func(_ context.Context, _ string, r *http.Request) (*http.Response, error) {
			require.Equal(t, Resource+"/models", r.URL.String())
			require.Equal(t, "Bearer t", r.Header.Get("Authorization"))
			require.Empty(t, r.Header.Get("x-openai-chatpass-test"))
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.raw))}, nil
		}
		raw, e := c.Catalog(context.Background(), "", "t")
		if !tc.valid {
			require.Error(t, e)
			continue
		}
		require.NoError(t, e)
		var v struct{ Data []any }
		require.NoError(t, json.Unmarshal(raw, &v))
		require.Len(t, v.Data, tc.count)
	}
}

func TestRefreshRotationConcurrentReplay(t *testing.T) {
	c := New()
	previous := Credential{Access: "a", Refresh: "old", IDToken: "verified", ClientID: "oaiapp_test", Subject: "s", IdentityKey: identityKey("oaiapp_test", "s"), GrantedScope: Scope}
	var count atomic.Int32
	c.do = func(context.Context, string, *http.Request) (*http.Response, error) {
		count.Add(1)
		return jsonResponse(t, map[string]any{"access_token": "new", "refresh_token": "rotated", "token_type": "Bearer", "expires_in": 3600}), nil
	}
	results := make(chan *Credential, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() { cred, err := c.Refresh(context.Background(), previous, ""); results <- cred; errs <- err }()
	}
	for i := 0; i < 8; i++ {
		require.NoError(t, <-errs)
		require.Equal(t, "rotated", (<-results).Refresh)
	}
	require.EqualValues(t, 1, count.Load())
}

func TestRefreshDoesNotBlockOtherIdentities(t *testing.T) {
	c := New()
	blocked, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	c.do = func(_ context.Context, _ string, r *http.Request) (*http.Response, error) {
		require.NoError(t, r.ParseForm())
		if r.Form.Get("client_id") == "oaiapp_first" {
			close(blocked)
			<-release
		}
		return jsonResponse(t, map[string]any{"access_token": "new", "refresh_token": "rotated", "token_type": "Bearer", "expires_in": 3600}), nil
	}
	previous := func(id string) Credential {
		return Credential{Refresh: "old", IDToken: "verified", ClientID: id, Subject: "s", IdentityKey: identityKey(id, "s"), GrantedScope: Scope}
	}
	first := make(chan error, 1)
	go func() { _, err := c.Refresh(context.Background(), previous("oaiapp_first"), ""); first <- err }()
	<-blocked
	second := make(chan error, 1)
	go func() { _, err := c.Refresh(context.Background(), previous("oaiapp_second"), ""); second <- err }()
	select {
	case err := <-second:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("another identity blocked on an unrelated refresh")
	}
	t.Cleanup(func() { require.NoError(t, <-first) })
}

func TestUpstreamErrorClassificationDoesNotExposeBodies(t *testing.T) {
	for _, tc := range []struct{ raw, code string }{
		{`{"error":"invalid_grant"}`, "invalid_grant"},
		{`{"error":{"code":"invalid_grant","message":"secret-token"}}`, "invalid_grant"},
		{`{"error":{"code":"unsupported_country_region_territory","message":"secret-token"}}`, "unsupported_country_region_territory"},
		{`{"error":{"code":"other","message":"secret-token"}}`, ""},
	} {
		c := New()
		c.do = func(context.Context, string, *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(tc.raw))}, nil
		}
		_, err := c.Catalog(context.Background(), "", "token")
		var upstream *HTTPError
		require.ErrorAs(t, err, &upstream)
		require.Equal(t, tc.code, upstream.Code)
		require.NotContains(t, err.Error(), "secret-token")
	}
}

func TestNestedInputRequiresInlineSupportedContent(t *testing.T) {
	for _, raw := range []string{
		`{"input":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"x"}}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file_old"}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"item_reference","id":"old"}]}]}`,
	} {
		_, err := NormalizePayload([]byte(raw))
		require.Error(t, err)
	}
	_, err := NormalizePayload([]byte(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,example"},{"type":"input_file","filename":"example.txt","file_data":"example"}]}]}`))
	require.NoError(t, err)
}
