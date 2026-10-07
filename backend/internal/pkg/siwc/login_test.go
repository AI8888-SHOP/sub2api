package siwc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type loginTransport func(*http.Request) (*http.Response, error)

func (f loginTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestTOTPStandardVector(t *testing.T) {
	code, e := TOTP("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", time.Unix(59, 0))
	require.NoError(t, e)
	require.Equal(t, "287082", code)
	_, e = TOTP("bad!", time.Now())
	require.Error(t, e)
}
func TestGoLoginPasswordTOTPConsentAndCallback(t *testing.T) {
	client := New()
	auth, e := client.Start("", nil)
	require.NoError(t, e)
	calls := []string{}
	transport := loginTransport(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "auth.openai.com", r.URL.Host)
		require.Equal(t, "node", r.Header.Get("User-Agent"))
		calls = append(calls, r.URL.Path)
		resp := &http.Response{StatusCode: 200, Header: make(http.Header)}
		raw := ""
		switch r.URL.Path {
		case "/api/accounts/authorize":
			raw = `<form method="post" action="/login-email"><input type="hidden" name="csrf" value="test"><input type="email" name="email"></form>`
		case "/login-email":
			require.NoError(t, r.ParseForm())
			require.Equal(t, "user@example.test", r.Form.Get("email"))
			require.Equal(t, "test", r.Form.Get("csrf"))
			raw = `{"continue_url":"/log-in/password"}`
		case "/log-in/password":
			raw = `<html>Password</html>`
		case "/api/accounts/password/verify":
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "pass", body["password"])
			raw = `{"oai-client-auth-session":{"mfa_challenge_factors":[{"factor_type":"totp","id":"factor"}]}}`
		case "/api/accounts/mfa/issue_challenge":
			raw = `{}`
		case "/api/accounts/mfa/verify":
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "factor", body["id"])
			require.Len(t, body["code"], 6)
			raw = `{"continue_url":"/consent"}`
		case "/consent":
			raw = `<form method="post" action="/consent-confirm">OpenClaw allowance<input type="hidden" name="csrf" value="consent-csrf"><input type="checkbox" name="token_sharing" value="true"><button type="submit" name="decision" value="allow">Allow</button></form>`
		case "/consent-confirm":
			require.NoError(t, r.ParseForm())
			require.Equal(t, "true", r.Form.Get("token_sharing"))
			require.Equal(t, "allow", r.Form.Get("decision"))
			resp.StatusCode = 302
			session := client.sessions[auth.SessionID]
			resp.Header.Set("Location", Redirect+"?state="+session.State+"&client_id=oaiapp_test&code=c")
		default:
			t.Fatalf("unexpected target: %s", r.URL.Path)
		}
		resp.Body = io.NopCloser(strings.NewReader(raw))
		return resp, nil
	})
	callback, e := loginWithClient(context.Background(), &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, auth.AuthURL, LoginInput{Email: "user@example.test", Password: "pass", TOTPSecret: "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"})
	require.NoError(t, e)
	require.True(t, strings.HasPrefix(callback, Redirect))
	require.Len(t, calls, 8)
}
func TestGoLoginStopsAtChallengeAndForeignRedirect(t *testing.T) {
	for _, challenge := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreign", true: "challenge"}[challenge], func(t *testing.T) {
			client := New()
			auth, e := client.Start("", nil)
			require.NoError(t, e)
			calls := 0
			transport := loginTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				require.Equal(t, "GET", r.Method)
				resp := &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://untrusted.example/login"}}, Body: io.NopCloser(strings.NewReader(""))}
				if challenge {
					resp.StatusCode = 200
					resp.Body = io.NopCloser(strings.NewReader("verify you are human CAPTCHA"))
				}
				return resp, nil
			})
			_, e = loginWithClient(context.Background(), &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, auth.AuthURL, LoginInput{Email: "user", Password: "secret"})
			require.ErrorIs(t, e, ErrManual)
			require.Equal(t, 1, calls)
		})
	}
}

func TestConsentSelectsApprovalAndRejectsUnknownControls(t *testing.T) {
	base, err := url.Parse(Issuer + "/consent")
	require.NoError(t, err)
	for _, tc := range []struct {
		name, controls string
		valid          bool
	}{
		{"approve_before_deny", `<button name="decision" value="allow">Allow</button><button name="decision" value="deny">Deny</button>`, true},
		{"approve_after_deny", `<button name="decision" value="deny">Deny</button><button name="decision" value="allow">Allow</button>`, true},
		{"deny_only", `<button name="decision" value="deny">Deny</button>`, false},
		{"conflicting_label", `<button name="decision" value="deny">Allow</button>`, false},
		{"unknown", `<button name="decision" value="continue">Continue</button>`, false},
		{"foreign_override", `<button name="decision" value="allow" formaction="https://foreign.example">Allow</button>`, false},
		{"unknown_select", `<button name="decision" value="allow">Allow</button><select name="workspace"><option>Other</option></select>`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`<form method="post" action="/consent">OpenClaw<input type="hidden" name="csrf" value="csrf"><input type="checkbox" name="token_sharing" value="true">` + tc.controls + `</form>`)
			_, values, stage, ok := loginForm(raw, base, LoginInput{})
			require.Equal(t, tc.valid, ok)
			if ok {
				require.Equal(t, "consent", stage)
				require.Equal(t, "allow", values.Get("decision"))
				require.Equal(t, "csrf", values.Get("csrf"))
			}
		})
	}
}
