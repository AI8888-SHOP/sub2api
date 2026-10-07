package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/siwc"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func testSiwcAccount() *Account {
	return &Account{ID: 41, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"auth_mode": "siwc", "auth_flow": siwc.Sharing, "granted_scope": siwc.Scope, "access_token": "token", "siwc_identity": "identity", "email": "same@example.test"}}
}
func TestSIWCRoutingAndCapabilities(t *testing.T) {
	account := testSiwcAccount()
	require.True(t, account.IsOpenAIOAuth())
	require.False(t, account.IsOpenAIOAuthLike())
	require.False(t, account.UsesOpenAICodexProtocol())
	require.True(t, account.IsOpenAIWSForceHTTPEnabled())
	require.False(t, account.IsCredentialUsableForShadow())
	for _, capability := range []OpenAIEndpointCapability{OpenAIEndpointCapabilityResponses, OpenAIEndpointCapabilityChatCompletions} {
		require.True(t, account.SupportsOpenAIEndpointCapability(capability))
	}
	require.False(t, account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityResponsesCompact))
	account.Extra = map[string]any{"openai_excel_bps": true}
	require.False(t, account.IsExcelBPSEnabled())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Request.Header.Set("chatgpt-account-id", "old-codex")
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.1")
	gateway := openAIClientToolsTestService(&httpUpstreamRecorder{})
	for _, passthrough := range []bool{true, false} {
		var req *http.Request
		var err error
		body := []byte(`{"model":"m","input":"hi"}`)
		if passthrough {
			req, err = gateway.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "token")
		} else {
			req, err = gateway.buildUpstreamRequest(context.Background(), c, account, body, "token", true, "", false)
		}
		require.NoError(t, err)
		require.Equal(t, siwc.Resource+"/responses", req.URL.String())
		require.Equal(t, siwc.UserAgent, req.Header.Get("User-Agent"))
		require.Empty(t, req.Header.Get("chatgpt-account-id"))
		require.Empty(t, req.Header.Get("originator"))
		raw, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.Contains(t, string(raw), `"stream":true`)
	}
	account.Credentials["auth_flow"] = siwc.Identity
	require.False(t, account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityResponses))
	_, err := gateway.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, []byte(`{}`), "t")
	require.ErrorContains(t, err, "sharing consent")
}
func TestSIWCForwardSSEAndNonStreaming(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(map[bool]string{true: "stream", false: "json"}[stream], func(t *testing.T) {
			events := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"m\",\"output\":[{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n"
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(events))}}
			gateway := openAIClientToolsTestService(upstream)
			account := testSiwcAccount()
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			body := []byte(`{"model":"m","input":"hi","stream":false}`)
			if stream {
				body = []byte(`{"model":"m","input":"hi","stream":true}`)
			}
			result, err := gateway.Forward(context.Background(), c, account, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, siwc.Resource+"/responses", upstream.lastReq.URL.String())
			require.Contains(t, rec.Body.String(), "hello")
			if !stream {
				require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
				require.NotContains(t, rec.Body.String(), "event:")
			}
		})
	}
}
func TestSIWCReauthRejectsCodexAndChangedIdentity(t *testing.T) {
	account := testSiwcAccount()
	require.Error(t, validateReauthToken(account, &OpenAITokenInfo{Email: "same@example.test"}))
	require.Error(t, validateReauthToken(account, &OpenAITokenInfo{Siwc: &siwc.Credential{IdentityKey: "other", AuthFlow: siwc.Sharing}}))
	require.NoError(t, validateReauthToken(account, &OpenAITokenInfo{Siwc: &siwc.Credential{IdentityKey: "identity", AuthFlow: siwc.Sharing}}))
}

func TestSIWCChatCompletionsUsesPublicResponses(t *testing.T) {
	events := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_chat\",\"status\":\"completed\",\"model\":\"m\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n"
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(events))}}
	gateway := openAIClientToolsTestService(upstream)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	result, err := gateway.ForwardAsChatCompletions(context.Background(), c, testSiwcAccount(), []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}`), "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, siwc.Resource+"/responses", upstream.lastReq.URL.String())
	require.Contains(t, rec.Body.String(), "hello")
	require.Contains(t, rec.Body.String(), "chat.completion")
}

func TestSIWCCredentialsNeverLeaveThroughLegacyWorker(t *testing.T) {
	svc, reader, repo, _, _, _ := newReauthTestService("acct-1")
	t.Cleanup(svc.oauth.Stop)
	reader.account.Credentials["auth_mode"] = "siwc"
	savePasswordReauthConfig(t, svc)
	_, err := repo.CreateTask(context.Background(), reader.account.ID, "snapshot")
	require.NoError(t, err)
	claim, err := svc.ClaimTask(context.Background(), "legacy-worker")
	require.NoError(t, err)
	require.Nil(t, claim)
	require.Equal(t, OpenAIOAuthReauthStatusFailed, repo.task.Status)
	require.Contains(t, repo.task.Error, "built-in Go")
}

func TestSIWCReauthDisallowsExternalEngineAndMailboxMode(t *testing.T) {
	svc, reader, _, _, _, _ := newReauthTestService("acct-1")
	t.Cleanup(svc.oauth.Stop)
	reader.account.Credentials["auth_mode"] = "siwc"
	for _, input := range []OpenAIOAuthReauthConfigInput{
		{LoginEmail: "user@example.com", CredentialMode: OpenAIOAuthReauthModePasswordTOTP, Password: "p", Engine: OpenAIOAuthReauthEngineSessionStudio},
		{LoginEmail: "user@example.com", CredentialMode: OpenAIOAuthReauthModeEmailOTPURL},
		{LoginEmail: "user@example.com", CredentialMode: OpenAIOAuthReauthModePasswordTOTP, Password: "p", ProxySource: OpenAIOAuthReauthProxySourceMihomo},
	} {
		_, err := svc.SaveCredentialConfig(context.Background(), reader.account.ID, input)
		require.Error(t, err)
	}
}

func TestSIWCCancelAndInvalidCallbackKeepSessionStateCoherent(t *testing.T) {
	svc := &OpenAIOAuthService{}
	auth, err := svc.GenerateSiwcAuth(context.Background(), nil, nil)
	require.NoError(t, err)
	_, err = svc.ExchangeSiwcCallback(context.Background(), auth.SessionID, "http://foreign.example/callback")
	require.Error(t, err)
	status, err := svc.SiwcLoginStatus(auth.SessionID)
	require.NoError(t, err)
	require.Equal(t, "requires_manual", status.Status)
	svc.CancelSiwcLogin(auth.SessionID)
	_, err = svc.ExchangeSiwcCallback(context.Background(), auth.SessionID, siwc.Redirect)
	require.ErrorContains(t, err, "expired")
	_, err = svc.SiwcLoginStatus(auth.SessionID)
	require.Error(t, err)

	state := svc.siwcState()
	state.jobs["running-exchange"] = &siwcJob{created: time.Now(), status: SiwcLoginStatus{Status: "exchanging"}}
	_, err = svc.ExchangeSiwcCallback(context.Background(), "running-exchange", siwc.Redirect)
	require.ErrorContains(t, err, "already exchanging")
	svc.CancelSiwcLogin("running-exchange")
}
