package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const prismBrowserMaxResponseBytes = 2 << 20

// ErrPrismBrowserResponseWritten marks a Prism failure whose HTTP response was
// already sent to the client. The gateway must not append a fallback error.
var ErrPrismBrowserResponseWritten = errors.New("Prism browser response already written")

func prismBrowserWrittenError(err error) error {
	return fmt.Errorf("%w: %v", ErrPrismBrowserResponseWritten, err)
}

func prismBrowserTerminal(body []byte, model string, stream bool) (string, error) {
	terminal := body
	if stream {
		terminal = nil
		for _, line := range bytes.Split(body, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if !gjson.ValidBytes(data) {
				return "", errors.New("prism adapter returned invalid SSE JSON")
			}
			switch gjson.GetBytes(data, "type").String() {
			case "response.created":
			case "response.completed":
				if terminal != nil {
					return "", errors.New("prism adapter returned repeated terminal events")
				}
				terminal = []byte(gjson.GetBytes(data, "response").Raw)
			default:
				return "", errors.New("prism adapter returned unsupported SSE event")
			}
		}
	}
	if !gjson.ValidBytes(terminal) || gjson.GetBytes(terminal, "status").String() != "completed" ||
		gjson.GetBytes(terminal, "model").String() != model ||
		gjson.GetBytes(terminal, "output.0.content.0.text").String() == "" ||
		gjson.GetBytes(terminal, "id").String() == "" {
		return "", errors.New("prism adapter returned an invalid terminal response")
	}
	return gjson.GetBytes(terminal, "id").String(), nil
}

func prismBrowserAdapterURL(baseURL string) (string, error) {
	parsed, err := url.Parse(prismBrowserResponsesURL(baseURL))
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", errors.New("prism adapter must use a local HTTP endpoint")
	}
	if ip := net.ParseIP(parsed.Hostname()); ip == nil || (!ip.Equal(net.ParseIP("127.0.0.1")) && !ip.Equal(net.IPv6loopback)) {
		return "", errors.New("prism adapter must bind to a numeric loopback address")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 || parsed.Path != "/v1/responses" {
		return "", errors.New("invalid Prism adapter endpoint")
	}
	return parsed.String(), nil
}

// prismBrowserAdapterMisconfigured reports the adapter's own authentication and
// routing failures: the gateway and adapter disagree on the bridge key or path.
// Account-level failures (for example prism_auth_required) must be returned to
// the caller unchanged so an operator can repair the selected Prism account.
func prismBrowserAdapterMisconfigured(status int, body []byte) bool {
	switch status {
	case http.StatusUnauthorized:
		return strings.TrimSpace(gjson.GetBytes(body, "error.type").String()) != "prism_auth_required"
	case http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed:
		return true
	default:
		return false
	}
}

// prismBrowserAdapterErrorMessage tells an admin why the adapter refused a test
// turn (unsupported model, busy browser, retained pending turn). The adapter only
// returns fixed error codes and messages, never credentials or prompt text.
func prismBrowserAdapterErrorMessage(status int, body []byte) string {
	code := strings.TrimSpace(gjson.GetBytes(body, "error.type").String())
	message := strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
	switch {
	case code != "" && message != "":
		return fmt.Sprintf("Prism adapter returned HTTP %d (%s): %s", status, code, truncateString(message, 300))
	case code != "":
		return fmt.Sprintf("Prism adapter returned HTTP %d (%s)", status, code)
	default:
		return fmt.Sprintf("Prism adapter returned HTTP %d", status)
	}
}

// prismBrowserCookie returns an explicitly captured Prism browser cookie. It
// intentionally does not fall back to the generic cookie credential, which is
// stripped during credential sanitization and may belong to another service.
func prismBrowserCookie(account *Account) string {
	if account == nil {
		return ""
	}
	if cookie := prismCredentialString(account, PrismCookieKey, "prism_Cookie"); cookie != "" {
		return cookie
	}
	raw := account.Credentials[PrismTemplateKey]
	var headers map[string]string
	switch value := raw.(type) {
	case map[string]any:
		headers = prismStringMap(value["headers"])
	case string:
		var decoded map[string]any
		if json.Unmarshal([]byte(value), &decoded) == nil {
			headers = prismStringMap(decoded["headers"])
		}
	}
	for key, value := range headers {
		if strings.EqualFold(key, "Cookie") {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (s *OpenAIGatewayService) forwardPrismBrowser(ctx context.Context, c *gin.Context, account *Account, body []byte, started time.Time) (*OpenAIForwardResult, error) {
	if isOpenAIResponsesCompactPath(c) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": "Prism adapter does not support responses/compact"}})
		return nil, prismBrowserWrittenError(errors.New("prism adapter does not support responses/compact"))
	}
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	stream := gjson.GetBytes(body, "stream").Bool()
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": "model is required"}})
		return nil, prismBrowserWrittenError(errors.New("prism adapter model is required"))
	}
	responseBody, upstreamHeaders, status, err := s.callPrismBrowser(ctx, account, body)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"type": "prism_unavailable", "message": "Prism adapter unavailable; request was not replayed"}})
		return nil, prismBrowserWrittenError(err)
	}
	if status != http.StatusOK {
		if prismBrowserAdapterMisconfigured(status, responseBody) {
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"type": "prism_unavailable", "message": "Prism adapter rejected the gateway; check the adapter key and endpoint"}})
			return nil, prismBrowserWrittenError(fmt.Errorf("prism adapter returned HTTP %d", status))
		}
		c.Data(status, "application/json", responseBody)
		return nil, prismBrowserWrittenError(fmt.Errorf("prism adapter returned HTTP %d", status))
	}
	responseID, err := prismBrowserTerminal(responseBody, model, stream)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"type": "invalid_prism_response", "message": "Prism adapter returned no valid terminal response"}})
		return nil, prismBrowserWrittenError(err)
	}
	contentType := "application/json"
	if stream {
		contentType = "text/event-stream"
	}
	SetActualOpenAIUpstreamEndpoint(c, "/v1/responses")
	c.Header("X-Prism-Usage", "unavailable")
	c.Data(http.StatusOK, contentType, responseBody)
	return &OpenAIForwardResult{
		RequestID:        responseID,
		ResponseID:       responseID,
		UpstreamHeaders:  upstreamHeaders,
		Model:            model,
		UpstreamModel:    model,
		Stream:           stream,
		Duration:         time.Since(started),
		UsageUnavailable: true,
	}, nil
}

func (s *OpenAIGatewayService) callPrismBrowser(ctx context.Context, account *Account, body []byte) ([]byte, http.Header, int, error) {
	if !accountUsesPrismBrowser(account, s.cfg) {
		return nil, nil, 0, errors.New("prism adapter is disabled; native fallback is prohibited")
	}
	endpoint, err := prismBrowserAdapterURL(s.cfg.Gateway.PrismBrowser.BaseURL)
	if err != nil {
		return nil, nil, 0, err
	}
	key := strings.TrimSpace(s.cfg.Gateway.PrismBrowser.APIKey)
	if key == "" {
		return nil, nil, 0, errors.New("prism adapter key is not configured")
	}
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, nil, 0, err
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, nil, 0, errors.New("invalid Prism OAuth token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Prism-Account-ID", strconv.FormatInt(account.ID, 10))
	req.Header.Set("X-Prism-OAuth-Token", token)
	if cookie := prismBrowserCookie(account); cookie != "" {
		if strings.ContainsAny(cookie, "\r\n") {
			return nil, nil, 0, errors.New("invalid Prism browser cookie")
		}
		req.Header.Set("X-Prism-Cookie", cookie)
	}
	// The token must never pass through an account proxy, environment proxy,
	// plugin transport, or an HTTP redirect.
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Timeout:       5 * time.Minute,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("prism adapter request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, prismBrowserMaxResponseBytes+1))
	if err != nil || len(responseBody) > prismBrowserMaxResponseBytes {
		return nil, nil, 0, errors.New("prism adapter response exceeded limit")
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, nil, 0, errors.New("prism adapter redirected unexpectedly")
	}
	return responseBody, resp.Header, resp.StatusCode, nil
}
