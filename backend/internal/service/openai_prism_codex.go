package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const PrismCodexEnabledKey = "openai_prism_codex"
const prismCodexUserAgent = "Codex/1.0 (OpenAI; Linux x86_64)"
const prismCodexBeta = "responses_websockets=2026-02-06"
const prismCodexProtocolHeader = "X-Sub2API-Upstream-Protocol"

// This is an independently implemented Codex compatibility profile, not a BPS
// endpoint or the unrelated Prism Web Agent protocol. Existing OAuth credentials
// remain authoritative. Old openai_prism_* / prism_template settings do not opt in.
func (a *Account) IsPrismCodexEnabled() bool {
	if a == nil {
		return false
	}
	enabled, _ := a.Extra[PrismCodexEnabledKey].(bool)
	return enabled && a.Platform == PlatformOpenAI && a.Type == AccountTypeOAuth &&
		!a.IsShadow() && !a.IsOpenAIAgentIdentity() && !a.IsOpenAIPersonalAccessToken()
}

func normalizePrismCodexExtra(extra map[string]any) error {
	if value, exists := extra[PrismCodexEnabledKey]; exists {
		enabled, ok := value.(bool)
		if !ok {
			return infraerrors.BadRequest("PRISM_CODEX_SETTINGS_INVALID", PrismCodexEnabledKey+" must be a boolean")
		}
		if enabled {
			if bps, _ := extra["openai_excel_bps"].(bool); bps {
				return infraerrors.BadRequest("OPENAI_PROTOCOL_CONFLICT", "Prism/Codex and Excel/Google Sheets BPS are mutually exclusive")
			}
			extra["openai_excel_bps"] = false
			extra[ExcelBPSAutoRecoverOn403Key] = false
			extra["openai_excel_bps_auto_disable_on_403"] = false
			extra[ExcelBPSAutoMoveOn403Key] = false
		}
	}
	if bps, _ := extra["openai_excel_bps"].(bool); bps {
		extra[PrismCodexEnabledKey] = false
	}
	return nil
}

func validatePrismCodexAccount(a *Account) error {
	if a == nil {
		return nil
	}
	if value, exists := a.Extra[PrismCodexEnabledKey]; exists {
		enabled, ok := value.(bool)
		if !ok {
			return infraerrors.BadRequest("PRISM_CODEX_SETTINGS_INVALID", PrismCodexEnabledKey+" must be a boolean")
		}
		if enabled && !a.IsPrismCodexEnabled() {
			return infraerrors.BadRequest("PRISM_CODEX_ACCOUNT_INVALID", "Prism/Codex requires a regular ChatGPT OAuth account")
		}
	}
	return nil
}

// Use raw JSON values so large image data, tool schemas, encrypted reasoning,
// tool-search payloads and numbers survive without typed-struct truncation.
func normalizePrismCodexBody(body []byte, websocket bool) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, errors.New("Prism/Codex requires a JSON object")
	}
	var model string
	if json.Unmarshal(fields["model"], &model) != nil || strings.TrimSpace(model) == "" {
		return nil, errors.New("Prism/Codex requires a model")
	}
	if prev, exists := fields["previous_response_id"]; exists {
		var id string
		if string(prev) != "null" && json.Unmarshal(prev, &id) != nil {
			return nil, errors.New("previous_response_id must be a string")
		}
		if id = strings.TrimSpace(id); id == "" {
			delete(fields, "previous_response_id")
		} else {
			fields["previous_response_id"], _ = json.Marshal(id)
		}
	}
	// These fields are not part of the Prism Codex Responses contract.
	for _, key := range []string{"user", "metadata", "stream_options", "prompt_cache_retention", "service_tier", "turnState", "type", "generate"} {
		delete(fields, key)
	}
	fields["store"] = json.RawMessage("false")
	fields["stream"] = json.RawMessage("true")
	if raw, ok := fields["instructions"]; !ok || string(raw) == "null" {
		fields["instructions"] = json.RawMessage(`""`)
	} else {
		var instructions string
		if json.Unmarshal(raw, &instructions) != nil {
			return nil, errors.New("instructions must be a string")
		}
	}
	if raw := fields["input"]; len(raw) == 0 || string(raw) == "null" {
		fields["input"] = json.RawMessage("[]")
	} else {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			fields["input"], _ = json.Marshal([]any{map[string]any{"role": "user", "content": text}})
		} else {
			var input []map[string]json.RawMessage
			if json.Unmarshal(raw, &input) != nil {
				return nil, errors.New("input must be a string or an array of items")
			}
			for _, item := range input {
				if item == nil {
					return nil, errors.New("input items must be objects")
				}
				var callID string
				if json.Unmarshal(item["call_id"], &callID) == nil && callID != "" && (len(callID) > 64 || strings.ContainsAny(callID, " \t\r\n")) {
					digest := sha256.Sum256([]byte(callID))
					item["call_id"], _ = json.Marshal("call_" + hex.EncodeToString(digest[:16]))
				}
			}
			fields["input"], _ = json.Marshal(input)
		}
	}
	if raw := fields["tools"]; len(raw) != 0 && string(raw) != "null" {
		var tools []map[string]json.RawMessage
		if json.Unmarshal(raw, &tools) != nil {
			return nil, errors.New("tools must be an array")
		}
		for _, tool := range tools {
			if tool == nil {
				return nil, errors.New("tools must contain objects")
			}
			var kind string
			_ = json.Unmarshal(tool["type"], &kind)
			if kind == "" {
				kind = "function"
				tool["type"] = json.RawMessage(`"function"`)
			}
			if kind != "function" {
				continue // Never strip options from hosted/custom/namespace tools.
			}
			if nested := tool["function"]; len(nested) != 0 {
				var fn map[string]json.RawMessage
				if json.Unmarshal(nested, &fn) != nil || fn == nil {
					return nil, errors.New("function tool must be an object")
				}
				for _, key := range []string{"name", "description", "parameters", "strict"} {
					if _, exists := tool[key]; !exists {
						if value, present := fn[key]; present {
							tool[key] = value
						}
					}
				}
				delete(tool, "function")
			}
			var name string
			if json.Unmarshal(tool["name"], &name) != nil || strings.TrimSpace(name) == "" {
				return nil, errors.New("function tool requires a name")
			}
		}
		fields["tools"], _ = json.Marshal(tools)
	}
	if raw := fields["reasoning"]; len(raw) > 0 && string(raw) != "null" {
		var include []string
		if v := fields["include"]; len(v) > 0 && json.Unmarshal(v, &include) != nil {
			return nil, errors.New("include must be an array of strings")
		}
		found := false
		for _, value := range include {
			found = found || value == "reasoning.encrypted_content"
		}
		if !found {
			include = append(include, "reasoning.encrypted_content")
		}
		fields["include"], _ = json.Marshal(include)
	}
	if websocket {
		fields["type"] = json.RawMessage(`"response.create"`)
		delete(fields, "stream")
		delete(fields, "store")
	}
	return json.Marshal(fields)
}

func prismCodexHeaders(token, accountID, turnState string) http.Header {
	h := http.Header{
		"Authorization":                     {"Bearer " + token},
		"User-Agent":                        {prismCodexUserAgent},
		"Originator":                        {"codex_cli_rs"},
		"Accept":                            {"text/event-stream"},
		"Content-Type":                      {"application/json"},
		"Accept-Language":                   {"en-US,en;q=0.9"},
		"Sec-Fetch-Dest":                    {"empty"},
		"Sec-Fetch-Mode":                    {"cors"},
		"Sec-Fetch-Site":                    {"same-origin"},
		"Openai-Beta":                       {prismCodexBeta},
		"X-Openai-Internal-Codex-Residency": {"us"},
	}
	if accountID != "" {
		h.Set("ChatGPT-Account-Id", accountID)
	}
	if turnState != "" {
		h.Set("X-Codex-Turn-State", turnState)
	}
	return h
}

// Reuse the core raw Responses/SSE response path without running the native
// Codex CLI transforms (which may drop continuation IDs or rewrite tools).
func (s *OpenAIGatewayService) forwardPrismCodex(ctx context.Context, c *gin.Context, account *Account, body []byte, start time.Time) (*OpenAIForwardResult, error) {
	// Prism's upstream only serves the standard Codex Responses route. The
	// legacy /responses/compact ingress is therefore lowered to the same
	// Responses body before Prism normalization, instead of being sent as a
	// nonexistent upstream sub-route or carrying compact-only fields through.
	if isOpenAIResponsesCompactPath(c) {
		normalizedCompact, changed, compactErr := normalizeOpenAICompactRequestBody(body)
		if compactErr != nil {
			MarkResponseCommitted(c)
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": compactErr.Error()}})
			return nil, compactErr
		}
		if changed {
			body = normalizedCompact
		}
		// Prism only exposes the standard Codex Responses route.  Lower the
		// legacy compact ingress to the native remote-compaction signal instead
		// of silently forwarding an ordinary Responses turn.
		body, compactErr = ensurePrismCodexCompactionTrigger(body)
		if compactErr != nil {
			MarkResponseCommitted(c)
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": compactErr.Error()}})
			return nil, compactErr
		}
	}
	view := newOpenAIRequestView(body)
	_, model := resolveOpenAIForwardMappedModels(account, view.Model, false)
	normalized, err := normalizePrismCodexBody(ReplaceModelInBody(body, model), false)
	if err != nil {
		MarkResponseCommitted(c)
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": err.Error()}})
		return nil, err
	}
	SetActualOpenAIUpstreamEndpoint(c, openAIResponsesUpstreamEndpoint)
	return s.forwardOpenAIPassthrough(ctx, c, account, normalized, body, view.Model, false,
		extractOpenAIReasoningEffortFromBody(normalized, model), view.Stream, start)
}

func ensurePrismCodexCompactionTrigger(body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, errors.New("Prism/Codex compact request requires a JSON object")
	}
	rawInput := fields["input"]
	var items []json.RawMessage
	if len(rawInput) > 0 && string(rawInput) != "null" {
		if err := json.Unmarshal(rawInput, &items); err != nil {
			var text string
			if json.Unmarshal(rawInput, &text) != nil {
				return nil, errors.New("Prism/Codex compact input must be a string or an array")
			}
			message, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": text})
			items = []json.RawMessage{message}
		}
	}
	for _, item := range items {
		if gjson.GetBytes(item, "type").String() == "compaction_trigger" {
			fields["input"], _ = json.Marshal(items)
			return json.Marshal(fields)
		}
	}
	items = append(items, json.RawMessage(`{"type":"compaction_trigger"}`))
	fields["input"], _ = json.Marshal(items)
	return json.Marshal(fields)
}

// Fail closed: a Prism attempt must not turn a transport/SSE error into an
// automatic scheduler retry on an account using another protocol.
func finishPrismCodexForward(c *gin.Context, err error) error {
	if err == nil {
		return nil
	}
	if !c.Writer.Written() && !IsResponseCommitted(c) {
		status := http.StatusBadGateway
		var failed *UpstreamFailoverError
		if errors.As(err, &failed) && failed.StatusCode >= 400 && failed.StatusCode <= 599 {
			status = failed.StatusCode
			if retryAfter := failed.ResponseHeaders.Get("Retry-After"); retryAfter != "" {
				c.Header("Retry-After", retryAfter)
			}
		}
		c.JSON(status, gin.H{"error": gin.H{"type": "upstream_error", "code": "prism_codex_upstream_error", "message": "Prism/Codex upstream request failed"}})
	}
	MarkResponseCommitted(c)
	return fmt.Errorf("Prism/Codex forwarding failed: %s", sanitizeUpstreamErrorMessage(err.Error()))
}

type prismCodexRequestSentError struct{ cause error }

func (e *prismCodexRequestSentError) Error() string {
	return "Prism/Codex response.create: " + e.cause.Error()
}
func (e *prismCodexRequestSentError) Unwrap() error { return e.cause }

func (s *OpenAIGatewayService) buildPrismCodexRequest(ctx context.Context, c *gin.Context, account *Account, body []byte, token string) (*http.Request, error) {
	// Prism currently exposes only the Codex Responses endpoint upstream. The
	// gateway's compact and image routes are compatibility surfaces that are
	// lowered to that same endpoint before this request is sent.
	normalized, err := normalizePrismCodexBody(body, false)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexURL, bytes.NewReader(normalized))
	if err != nil {
		return nil, err
	}
	turnState := ""
	if c != nil && c.Request != nil {
		turnState = c.GetHeader("X-Codex-Turn-State")
		c.Header(prismCodexProtocolHeader, "prism_codex")
	}
	req.Header = prismCodexHeaders(token, excelBPSAccountID(account, token), turnState)
	s.guardOpenAICodexTurnStateEcho(c, account, req.Header)
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	return req, nil
}

// Always run inside the existing egress/RPM attempt wrapper. No BPS, plugin,
// ticket or native-WS-pool fallback is permitted by this profile.
func (s *OpenAIGatewayService) doPrismCodexUpstream(req *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.URL.Host != "chatgpt.com" || req.URL.Path != "/backend-api/codex/responses" || req.Method != http.MethodPost {
		return nil, errors.New("Prism/Codex refused an unexpected upstream endpoint")
	}
	if req.GetBody == nil {
		return nil, errors.New("Prism/Codex requires a replayable request body")
	}
	reader, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		return nil, err
	}
	body, err = normalizePrismCodexBody(body, false)
	if err != nil {
		return nil, err
	}
	// Probes and WS ingress may construct requests through different builders.
	// Reapply the profile at the last send boundary, never forwarding cookies,
	// client credentials, legacy tickets or plugin-specific headers.
	prepared, err := http.NewRequestWithContext(req.Context(), http.MethodPost, chatgptCodexURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	authorization := req.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer ")) == "" {
		return nil, errors.New("Prism/Codex requires an OAuth access token")
	}
	token := strings.TrimPrefix(authorization, "Bearer ")
	prepared.Header = prismCodexHeaders(token, excelBPSAccountID(account, token), req.Header.Get("X-Codex-Turn-State"))
	req = prepared
	if strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) == "" {
		return s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	}
	frame, err := normalizePrismCodexBody(body, true)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(req.Context())
	dialCtx, dialCancel := context.WithTimeout(ctx, 20*time.Second)
	conn, status, headers, err := s.getOpenAIWSPassthroughDialer().Dial(dialCtx, "wss://chatgpt.com/backend-api/codex/responses", req.Header, proxyURL)
	dialCancel()
	if err != nil {
		cancel()
		if status >= 400 {
			raw := []byte(`{"error":{"message":"Prism/Codex WebSocket handshake rejected","type":"upstream_error"}}`)
			var handshake *openAIWSHandshakeError
			if errors.As(err, &handshake) && len(handshake.Body) > 0 {
				raw = handshake.Body
			}
			return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(bytes.NewReader(raw)), Request: req}, nil
		}
		return nil, err
	}
	writeCtx, writeCancel := context.WithTimeout(ctx, 20*time.Second)
	err = conn.WriteJSON(writeCtx, json.RawMessage(frame))
	writeCancel()
	if err != nil {
		cancel()
		_ = conn.Close()
		return nil, &prismCodexRequestSentError{cause: err}
	}
	pr, pw := io.Pipe()
	stream := &prismCodexStream{PipeReader: pr, cancel: cancel, conn: conn}
	stop := context.AfterFunc(ctx, func() {
		_ = pw.CloseWithError(ctx.Err())
		stream.closeConn()
	})
	go func() {
		defer stop()
		defer cancel()
		defer stream.closeConn()
		defer func() { _ = pw.Close() }()
		for {
			readCtx, readCancel := context.WithTimeout(ctx, s.openAIWSReadTimeout())
			raw, readErr := conn.ReadMessage(readCtx)
			readCancel()
			if readErr != nil {
				_ = pw.CloseWithError(fmt.Errorf("Prism/Codex stream ended before a terminal event: %w", readErr))
				return
			}
			var event struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(raw, &event) != nil || event.Type == "" || strings.ContainsAny(event.Type, "\r\n") {
				_ = pw.CloseWithError(errors.New("invalid Prism/Codex stream event"))
				return
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, raw); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if _, err := fmt.Fprintf(pw, "event: %s\ndata: %s\n\n", event.Type, compact.Bytes()); err != nil {
				return
			}
			switch event.Type {
			case "response.completed", "response.done", "response.failed", "response.incomplete", "error":
				return
			}
		}
	}()
	if headers == nil {
		headers = make(http.Header)
	} else {
		headers = headers.Clone()
	}
	headers.Set("Content-Type", "text/event-stream")
	for _, key := range []string{"Connection", "Upgrade", "Sec-WebSocket-Accept", "Sec-WebSocket-Extensions", "Sec-WebSocket-Protocol", "Content-Length"} {
		headers.Del(key)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: stream, Request: req}, nil
}

type prismCodexStream struct {
	*io.PipeReader
	cancel    context.CancelFunc
	conn      openAIWSClientConn
	closeOnce sync.Once
}

func (s *prismCodexStream) closeConn() {
	s.closeOnce.Do(func() {
		if force, ok := s.conn.(openAIWSForceCloser); ok {
			_ = force.CloseNow()
		} else {
			_ = s.conn.Close()
		}
	})
}
func (s *prismCodexStream) Close() error {
	s.cancel()
	err := s.PipeReader.Close()
	s.closeConn()
	return err
}

func rejectUnsupportedPrismCodexEndpoint(c *gin.Context, account *Account) error {
	if err := validatePrismCodexAccount(account); err != nil {
		MarkResponseCommitted(c)
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "code": "prism_codex_configuration_error", "message": err.Error()}})
		return err
	}
	if !account.IsPrismCodexEnabled() {
		return nil
	}
	c.Header(prismCodexProtocolHeader, "prism_codex")
	path := ""
	if c != nil && c.Request != nil && c.Request.URL != nil {
		path = c.Request.URL.Path
	}
	if strings.HasSuffix(path, "/responses") || strings.HasSuffix(path, "/chat/completions") ||
		isOpenAIResponsesCompactPath(c) ||
		IsOpenAIResponsesInputTokensRequestPath(c) ||
		strings.HasSuffix(path, "/images/generations") ||
		strings.HasSuffix(path, "/images/edits") {
		return nil
	}
	err := errors.New("Prism/Codex does not support this endpoint")
	MarkResponseCommitted(c)
	c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "code": "prism_codex_endpoint_unsupported", "message": err.Error()}})
	return err
}
