//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func prismTestAccount() *Account {
	return &Account{ID: 41, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Concurrency: 2,
		Credentials: map[string]any{"access_token": "test-oauth", "chatgpt_account_id": "test-account"},
		Extra:       map[string]any{PrismCodexEnabledKey: true}}
}

func TestPrismCodexNormalization(t *testing.T) {
	callID := strings.Repeat("long-call-", 12)
	input := `{"model":"gpt-6-astra","stream":false,"store":true,"service_tier":"priority","metadata":{"drop":true},"previous_response_id":"resp_prev","reasoning":{"effort":"high"},"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AQID","detail":"high"}]},{"type":"reasoning","encrypted_content":"encrypted-preserved"},{"type":"function_call","call_id":"` + callID + `","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"` + callID + `","output":"ok"}],"tools":[{"type":"function","function":{"name":"lookup","strict":true,"parameters":{"type":"object","properties":{"n":{"const":9007199254740993}}}}},{"type":"image_generation","quality":"high"}]}`
	for _, ws := range []bool{false, true} {
		body, err := normalizePrismCodexBody([]byte(input), ws)
		require.NoError(t, err)
		require.Equal(t, "resp_prev", gjson.GetBytes(body, "previous_response_id").String())
		require.Equal(t, "data:image/png;base64,AQID", gjson.GetBytes(body, "input.0.content.0.image_url").String())
		require.Equal(t, "encrypted-preserved", gjson.GetBytes(body, "input.1.encrypted_content").String())
		require.Equal(t, gjson.GetBytes(body, "input.2.call_id").String(), gjson.GetBytes(body, "input.3.call_id").String())
		require.LessOrEqual(t, len(gjson.GetBytes(body, "input.2.call_id").String()), 64)
		require.Equal(t, "9007199254740993", gjson.GetBytes(body, "tools.0.parameters.properties.n.const").Raw)
		require.Equal(t, "lookup", gjson.GetBytes(body, "tools.0.name").String())
		require.Equal(t, "high", gjson.GetBytes(body, "tools.1.quality").String())
		require.Equal(t, "reasoning.encrypted_content", gjson.GetBytes(body, "include.0").String())
		require.False(t, gjson.GetBytes(body, "service_tier").Exists())
		require.False(t, gjson.GetBytes(body, "metadata").Exists())
		if ws {
			require.Equal(t, "response.create", gjson.GetBytes(body, "type").String())
			require.False(t, gjson.GetBytes(body, "stream").Exists())
			require.False(t, gjson.GetBytes(body, "store").Exists())
		} else {
			require.True(t, gjson.GetBytes(body, "stream").Bool())
			require.False(t, gjson.GetBytes(body, "store").Bool())
		}
	}
	for _, input := range []string{`null`, `[]`, `{}`, `{"model":"m","input":[null]}`, `{"model":"m","tools":[null]}`, `{"model":"m","tools":[{"function":null}]}`, `{"model":"m","previous_response_id":42}`} {
		t.Run(input, func(t *testing.T) { _, err := normalizePrismCodexBody([]byte(input), false); require.Error(t, err) })
	}
}

func TestPrismCodexSettingsAndRouting(t *testing.T) {
	a := prismTestAccount()
	a.Extra["openai_excel_bps"] = true // Legacy/conflicting data still cannot route BPS.
	a.Extra["openai_passthrough"] = true
	require.True(t, a.IsPrismCodexEnabled())
	require.False(t, a.IsExcelBPSEnabled())
	require.False(t, a.IsOpenAIPassthroughEnabled())
	require.False(t, QualityBPSEligible(a))
	require.False(t, isOpenAICodexTicketAccount(a))
	require.False(t, a.IsOpenAIResponsesWebSocketV2Enabled())
	require.Equal(t, OpenAIUpstreamTransportHTTPSSE, NewOpenAIWSProtocolResolver(nil).Resolve(a).Transport)
	require.Error(t, normalizePrismCodexExtra(a.Extra))
	a.Extra = map[string]any{PrismCodexEnabledKey: true}
	require.NoError(t, normalizePrismCodexExtra(a.Extra))
	require.Equal(t, false, a.Extra["openai_excel_bps"])
	require.Equal(t, false, a.Extra[ExcelBPSAutoRecoverOn403Key])
	a.Extra = map[string]any{"openai_excel_bps": true}
	require.NoError(t, normalizePrismCodexExtra(a.Extra))
	require.Equal(t, false, a.Extra[PrismCodexEnabledKey])
	for _, mode := range []string{"agentidentity", "personal_access_token", "personalaccesstoken"} {
		a := prismTestAccount()
		a.Credentials["auth_mode"] = mode
		require.Error(t, validatePrismCodexAccount(a))
	}
	a = prismTestAccount()
	a.Type = AccountTypeAPIKey
	require.Error(t, validatePrismCodexAccount(a))
	a = prismTestAccount()
	a.Extra = map[string]any{"openai_prism": true, "openai_prism_web_agent": true}
	require.False(t, a.IsPrismCodexEnabled(), "old Web Agent settings must not opt in")
	a = prismTestAccount()
	before := openAITurnRouteFingerprint(a)
	a.Extra[PrismCodexEnabledKey] = false
	require.NotEqual(t, before, openAITurnRouteFingerprint(a))
}

type prismHTTPRecorder struct {
	HTTPUpstream
	req    *http.Request
	body   []byte
	calls  int
	status int
}

const prismCompleted = `{"type":"response.completed","response":{"id":"resp_prism","object":"response","model":"gpt-6-astra","status":"completed","created_at":1790852626,"output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"pong","annotations":[]}]}],"usage":{"input_tokens":9,"output_tokens":2,"total_tokens":11}}}`

func (u *prismHTTPRecorder) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	u.req = req
	var err error
	u.body, err = io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	status := u.status
	if status == 0 {
		status = 200
	}
	body := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\",\"output_index\":0,\"content_index\":0}\n\nevent: response.completed\ndata: " + prismCompleted + "\n\n"
	if status >= 400 {
		body = `{"error":{"type":"upstream_error","message":"denied"}}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func TestPrismCodexHTTPProfileAndFullForward(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/chat/completions"} {
		for _, stream := range []bool{false, true} {
			t.Run(path+map[bool]string{false: "/json", true: "/sse"}[stream], func(t *testing.T) {
				a := prismTestAccount()
				a.Extra["openai_excel_bps"] = true
				u := &prismHTTPRecorder{}
				s := &OpenAIGatewayService{httpUpstream: u, accountRepo: &turnAdmissionRepo{account: a}, cfg: &config.Config{RunMode: config.RunModeSimple}}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, path, nil)
				c.Request.Header.Set("Cookie", "must-not-forward")
				c.Request.Header.Set("User-Agent", "wrong-client")
				body := `{"model":"gpt-6-astra","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AQID"}]}],"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `}`
				var result *OpenAIForwardResult
				var err error
				if strings.Contains(path, "chat/completions") {
					body = `{"model":"gpt-6-astra","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AQID"}}]}],"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `}`
					result, err = s.ForwardAsChatCompletions(context.Background(), c, a, []byte(body), "", "")
				} else {
					result, err = s.Forward(context.Background(), c, a, []byte(body))
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, 1, u.calls)
				require.Equal(t, chatgptCodexURL, u.req.URL.String())
				require.Equal(t, prismCodexUserAgent, u.req.Header.Get("User-Agent"))
				require.Empty(t, u.req.Header.Get("Cookie"))
				require.Equal(t, "Bearer test-oauth", u.req.Header.Get("Authorization"))
				require.Equal(t, "test-account", u.req.Header.Get("ChatGPT-Account-Id"))
				require.Equal(t, prismCodexBeta, u.req.Header.Get("OpenAI-Beta"))
				require.True(t, gjson.GetBytes(u.body, "stream").Bool())
				require.Contains(t, string(u.body), "data:image/png;base64,AQID")
				require.Equal(t, stream, result.Stream)
				require.Equal(t, 9, result.Usage.InputTokens)
				require.Equal(t, "prism_codex", rec.Header().Get(prismCodexProtocolHeader))
				require.Contains(t, rec.Body.String(), "pong")
				if !stream {
					require.True(t, json.Valid(rec.Body.Bytes()), rec.Body.String())
				}
			})
		}
	}
}

type prismWSConn struct {
	frames   [][]byte
	frame    []byte
	closed   chan struct{}
	once     sync.Once
	writeErr error
}

func (c *prismWSConn) WriteJSON(_ context.Context, v any) error {
	c.frame, _ = json.Marshal(v)
	return c.writeErr
}
func (c *prismWSConn) ReadMessage(ctx context.Context) ([]byte, error) {
	if len(c.frames) > 0 {
		f := c.frames[0]
		c.frames = c.frames[1:]
		return f, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, io.EOF
	}
}
func (c *prismWSConn) Ping(context.Context) error { return nil }
func (c *prismWSConn) Close() error               { c.once.Do(func() { close(c.closed) }); return nil }

func TestPrismCodexWebSocketContinuation(t *testing.T) {
	for _, terminal := range []string{prismCompleted, `{"type":"response.failed","response":{"error":{"message":"denied"}}}`, `{"type":"error","error":{"message":"denied"}}`} {
		a := prismTestAccount()
		conn := &prismWSConn{frames: [][]byte{[]byte(`{"type":"response.function_call_arguments.delta","delta":"{}"}`), []byte(terminal)}, closed: make(chan struct{})}
		u := &prismHTTPRecorder{}
		s := &OpenAIGatewayService{httpUpstream: u}
		s.openaiWSPassthroughDialer = turnAdmissionDialerFunc(func(_ context.Context, url string, h http.Header, proxy string) (openAIWSClientConn, int, http.Header, error) {
			require.Equal(t, "wss://chatgpt.com/backend-api/codex/responses", url)
			require.Equal(t, "http://proxy.invalid:80", proxy)
			require.Equal(t, "Bearer test-oauth", h.Get("Authorization"))
			require.Empty(t, h.Get("Cookie"))
			return conn, 101, http.Header{"X-Codex-Turn-State": {"new-state"}}, nil
		})
		req, err := s.buildPrismCodexRequest(context.Background(), nil, a, []byte(`{"model":"gpt-6-astra","previous_response_id":"resp_prev","input":[{"type":"function_call_output","call_id":"call_x","output":"ok"}]}`), "test-oauth")
		require.NoError(t, err)
		resp, err := s.doPrismCodexUpstream(req, "http://proxy.invalid:80", a)
		require.NoError(t, err)
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, 0, u.calls)
		require.Contains(t, string(raw), terminal)
		require.Contains(t, string(raw), "response.function_call_arguments.delta")
		require.Equal(t, "response.create", gjson.GetBytes(conn.frame, "type").String())
		require.Equal(t, "resp_prev", gjson.GetBytes(conn.frame, "previous_response_id").String())
		require.Equal(t, "call_x", gjson.GetBytes(conn.frame, "input.0.call_id").String())
		require.False(t, gjson.GetBytes(conn.frame, "stream").Exists())
		select {
		case <-conn.closed:
		case <-time.After(time.Second):
			t.Fatal("connection leaked")
		}
	}
}

func TestPrismCodexWebSocketCancellationAndNoReplay(t *testing.T) {
	for _, failWrite := range []bool{false, true} {
		a := prismTestAccount()
		id := int64(5)
		a.ProxyID = &id
		a.Proxy = proxyForTest(id, "primary.invalid", 80)
		a.Proxy.FallbackMode = FallbackModeDirect
		conn := &prismWSConn{closed: make(chan struct{})}
		if failWrite {
			conn.writeErr = errors.New("connection reset by peer")
		}
		s := &OpenAIGatewayService{}
		calls := 0
		s.openaiWSPassthroughDialer = turnAdmissionDialerFunc(func(ctx context.Context, _ string, _ http.Header, _ string) (openAIWSClientConn, int, http.Header, error) {
			calls++
			if tr := httptrace.ContextClientTrace(ctx); tr != nil && tr.GetConn != nil {
				tr.GetConn("chatgpt.com:443")
			}
			return conn, 101, nil, nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		req, err := s.buildPrismCodexRequest(ctx, nil, a, []byte(`{"model":"m","previous_response_id":"resp_prev"}`), "test-oauth")
		require.NoError(t, err)
		resp, err := s.doOpenAIUpstream(req, a.Proxy.URL(), a)
		if failWrite {
			require.Error(t, err)
			require.Nil(t, resp)
		} else {
			require.NoError(t, err)
			cancel()
			_, err = io.ReadAll(resp.Body)
			require.Error(t, err)
			require.NoError(t, resp.Body.Close())
		}
		cancel()
		require.Equal(t, 1, calls)
		select {
		case <-conn.closed:
		case <-time.After(time.Second):
			t.Fatal("connection leaked")
		}
	}
}

func TestPrismCodexUnsupportedEndpointsAndNoMixedErrors(t *testing.T) {
	for _, path := range []string{"/v1/images/generations", "/v1/images/edits", "/v1/responses/compact", "/v1/responses/input_tokens"} {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest("POST", path, nil)
		err := rejectUnsupportedPrismCodexEndpoint(c, prismTestAccount())
		require.Error(t, err)
		err = finishPrismCodexForward(c, err)
		require.Error(t, err)
		require.Equal(t, 400, r.Code)
		require.True(t, json.Valid(r.Body.Bytes()))
		require.True(t, IsResponseCommitted(c))
	}
	for _, status := range []int{403, 429} {
		s := &OpenAIGatewayService{openaiWSPassthroughDialer: turnAdmissionDialerFunc(func(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error) {
			return nil, status, http.Header{"Retry-After": {"3"}}, errors.New("handshake failed")
		})}
		req, err := s.buildPrismCodexRequest(context.Background(), nil, prismTestAccount(), []byte(`{"model":"m","previous_response_id":"prev"}`), "test-oauth")
		require.NoError(t, err)
		resp, err := s.doPrismCodexUpstream(req, "", prismTestAccount())
		require.NoError(t, err)
		require.Equal(t, status, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
	}
}
