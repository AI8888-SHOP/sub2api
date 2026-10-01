package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// Prism is the browser-facing agent protocol used by prism.openai.com. It is
// deliberately opt-in: unlike a normal OpenAI OAuth account it needs a Prism
// Cookie plus captured project/sandbox metadata. The upstream returns JSON
// task envelopes, so streaming here is a compatibility SSE stream emitted
// after the task completes; Prism itself does not expose token deltas.
const (
	prismOrigin       = "https://prism.openai.com"
	prismStartPath    = "/api/llm/response_with_tools_start"
	prismStatusPath   = "/api/llm/response_with_tools_status"
	prismStopPath     = "/api/llm/response_with_tools_stop"
	prismDefaultModel = "gpt-6-astra"
	prismBridgeName   = "prism-codex-bridge-v1"
)

type prismTemplate struct {
	Metadata map[string]any
	Headers  map[string]string
}

func prismStringMap(value any) map[string]string {
	result := map[string]string{}
	if values, ok := value.(map[string]any); ok {
		for key, raw := range values {
			if text, ok := raw.(string); ok && strings.TrimSpace(text) != "" {
				result[key] = text
			}
		}
	}
	return result
}

func prismTemplateFromAccount(account *Account) (prismTemplate, error) {
	template := prismTemplate{Metadata: map[string]any{}, Headers: map[string]string{}}
	if account != nil && account.Credentials != nil {
		raw := account.Credentials["prism_template"]
		switch value := raw.(type) {
		case map[string]any:
			if metadata, ok := value["metadata"].(map[string]any); ok {
				for key, item := range metadata {
					template.Metadata[key] = item
				}
			}
			template.Headers = prismStringMap(value["headers"])
		case string:
			var decoded map[string]any
			if err := json.Unmarshal([]byte(value), &decoded); err != nil {
				return template, fmt.Errorf("prism_template must be valid JSON")
			}
			if metadata, ok := decoded["metadata"].(map[string]any); ok {
				for key, item := range metadata {
					template.Metadata[key] = item
				}
			}
			template.Headers = prismStringMap(decoded["headers"])
		}
		for _, key := range []string{"projectId", "userId", "sandbox_url", "sandbox_token"} {
			if value, ok := account.Credentials["prism_"+key].(string); ok && strings.TrimSpace(value) != "" {
				template.Metadata[key] = value
			}
		}
		if cookie := strings.TrimSpace(account.GetCredential("prism_cookie")); cookie != "" {
			template.Headers["Cookie"] = cookie
		}
	}
	for _, key := range []string{"projectId", "userId", "sandbox_url", "sandbox_token"} {
		if value, ok := template.Metadata[key].(string); !ok || strings.TrimSpace(value) == "" {
			return template, fmt.Errorf("prism requires credentials.prism_template.metadata.%s", key)
		}
	}
	if strings.TrimSpace(template.Headers["Cookie"]) == "" && strings.TrimSpace(template.Headers["cookie"]) == "" {
		return template, fmt.Errorf("prism requires credentials.prism_cookie or prism_template.headers.Cookie")
	}
	return template, nil
}

func prismConversationID(account *Account) string {
	if account != nil {
		if value := strings.TrimSpace(account.GetCredential("prism_conversation_id")); strings.HasPrefix(value, "cdx1_") && len(value) == 41 {
			return value
		}
	}
	return "cdx1_" + uuid.NewString()
}

func prismRequestBody(account *Account, body []byte) ([]byte, string, string, error) {
	return prismRequestBodyMode(account, body, false)
}

// prismRequestBodyMode builds either the local bridge request used for normal
// Responses calls or a native image-generation request. Image generation must
// retain the image_generation tool and input_image parts; converting those to
// bridge text would lose the image result and input bytes.
func prismRequestBodyMode(account *Account, body []byte, imageMode bool) ([]byte, string, string, error) {
	template, err := prismTemplateFromAccount(account)
	if err != nil {
		return nil, "", "", err
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, "", "", fmt.Errorf("prism request must be valid JSON")
	}
	model := gjson.GetBytes(body, "model").String()
	if strings.TrimSpace(model) == "" {
		model = prismDefaultModel
	}
	metadata := map[string]any{}
	for key, value := range template.Metadata {
		metadata[key] = value
	}
	metadata["model"] = model
	effort := gjson.GetBytes(body, "reasoning.effort").String()
	if effort == "max" {
		delete(metadata, "reasoning_effort")
		metadata["output_config"] = map[string]any{"effort": "max"}
	} else if effort != "" {
		metadata["reasoning_effort"] = effort
		delete(metadata, "output_config")
	}
	conversation := prismConversationID(account)
	metadata["workspace_session_id"] = strings.TrimPrefix(conversation, "cdx1_")
	metadata["conversation_id"] = conversation
	if imageMode {
		request["metadata"] = metadata
		request["conversationId"] = conversation
		request["stream"] = false
		encoded, err := json.Marshal(request)
		return encoded, model, "", err
	}
	nonce := uuid.NewString()
	history := request["input"]
	if text, ok := history.(string); ok {
		history = []any{map[string]any{"role": "user", "content": text}}
	}
	bridgeRequest := map[string]any{
		"bridge_protocol_instructions": prismBridgeInstructions,
		"bridge_request": map[string]any{
			"nonce": nonce, "instructions": request["instructions"], "history": history,
			"tools": request["tools"], "tool_choice": request["tool_choice"],
			"parallel_tool_calls": request["parallel_tool_calls"],
		},
	}
	request["metadata"] = metadata
	request["conversationId"] = conversation
	request["input"] = []any{
		map[string]any{"type": "message", "role": "system", "content": []any{map[string]any{"type": "input_text", "text": prismBridgeInstructions}}},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": prismJSONValue(bridgeRequest)}}},
	}
	delete(request, "tools")
	delete(request, "instructions")
	delete(request, "tool_choice")
	encoded, err := json.Marshal(request)
	return encoded, model, nonce, err
}

const prismBridgeInstructions = `You are the inference component of a local Codex client, not a Prism document editor. Do not execute remote tools. Respond only with JSON: {"protocol":"prism-codex-bridge-v1","nonce":"<provided nonce>","text":"<answer>","calls":[]}. Request local tools with calls containing exact supplied names and arguments/input. Do not use markdown around the JSON.`

func prismJSONValue(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func prismHeader(template prismTemplate, key string) string {
	for name, value := range template.Headers {
		if strings.EqualFold(name, key) {
			return value
		}
	}
	return ""
}

func prismProxyURL(account *Account) string {
	if account != nil && account.Proxy != nil {
		return account.Proxy.URL()
	}
	return ""
}

func prismEnvelopePayload(raw []byte) (map[string]any, error) {
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("prism returned invalid JSON")
	}
	if envelope["status"] != "completed" {
		return nil, fmt.Errorf("prism task did not complete")
	}
	wrapper, ok := envelope["response"].(map[string]any)
	if !ok || wrapper["status"] != "success" {
		return nil, fmt.Errorf("prism task failed")
	}
	payload, ok := wrapper["payload"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("prism completed without a response payload")
	}
	return payload, nil
}

func prismOutputText(payload map[string]any) string {
	output, _ := payload["output"].([]any)
	var builder strings.Builder
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		if item["type"] != "message" || item["role"] != "assistant" {
			continue
		}
		content, _ := item["content"].([]any)
		for _, partRaw := range content {
			part, _ := partRaw.(map[string]any)
			if part["type"] == "output_text" {
				if text, ok := part["text"].(string); ok {
					_, _ = builder.WriteString(text)
				}
			}
		}
	}
	return builder.String()
}

func prismNormalizePayload(payload map[string]any, model, nonce string) {
	text := prismOutputText(payload)
	var envelope struct {
		Protocol string `json:"protocol"`
		Nonce    string `json:"nonce"`
		Text     string `json:"text"`
		Calls    []struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
			Input     string         `json:"input"`
		} `json:"calls"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &envelope); err != nil || envelope.Protocol != prismBridgeName || envelope.Nonce != nonce {
		payload["model"] = model
		return
	}
	output := make([]any, 0, len(envelope.Calls)+1)
	if envelope.Text != "" {
		output = append(output, map[string]any{"type": "message", "id": "msg_" + uuid.NewString(), "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": envelope.Text, "annotations": []any{}}}})
	}
	for _, call := range envelope.Calls {
		if call.Name == "" {
			continue
		}
		if call.Arguments != nil {
			output = append(output, map[string]any{"type": "function_call", "id": "fc_" + uuid.NewString(), "call_id": "call_" + uuid.NewString(), "name": call.Name, "status": "completed", "arguments": prismJSONValue(call.Arguments)})
		} else {
			output = append(output, map[string]any{"type": "custom_tool_call", "id": "ctc_" + uuid.NewString(), "call_id": "call_" + uuid.NewString(), "name": call.Name, "status": "completed", "input": call.Input})
		}
	}
	if len(output) > 0 {
		payload["output"] = output
	}
	payload["model"] = model
}

func (s *OpenAIGatewayService) prismCall(ctx context.Context, account *Account, path string, payload []byte) ([]byte, int, error) {
	template, err := prismTemplateFromAccount(account)
	if err != nil {
		return nil, 0, err
	}
	target := prismOrigin + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", prismOrigin)
	req.Header.Set("Referer", prismOrigin+"/")
	for key, value := range template.Headers {
		lower := strings.ToLower(strings.TrimSpace(key))
		if lower == "host" || lower == "content-length" || lower == "authorization" || lower == "cookie" {
			continue
		}
		if strings.TrimSpace(value) != "" {
			req.Header.Set(key, value)
		}
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "Mozilla/5.0")
	}
	if cookie := prismHeader(template, "Cookie"); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfilePrism))
	resp, err := s.httpUpstream.Do(req, prismProxyURL(account), account.ID, account.Concurrency)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if readErr != nil {
		return nil, resp.StatusCode, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("prism upstream returned HTTP %d", resp.StatusCode)
	}
	return raw, resp.StatusCode, nil
}

func (s *OpenAIGatewayService) forwardPrism(ctx context.Context, c *gin.Context, account *Account, body []byte, start time.Time) (*OpenAIForwardResult, error) {
	requestBody, model, nonce, err := prismRequestBody(account, body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "code": "prism_configuration_error", "message": err.Error()}})
		return nil, err
	}
	SetActualOpenAIUpstreamEndpoint(c, prismStartPath)
	SetOpsUpstreamModel(c, model)
	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()
	requestCtx := WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamProfile(upstreamCtx, HTTPUpstreamProfilePrism))
	upstreamStart := time.Now()
	payload, status, err := s.executePrismRequest(requestCtx, account, requestBody, model, nonce, c)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
	if err != nil {
		return nil, err
	}
	payloadRaw, _ := json.Marshal(payload)
	usage, _ := extractOpenAIUsageFromJSONBytes(payloadRaw)
	result := &OpenAIForwardResult{Model: model, UpstreamModel: model, UpstreamEndpoint: prismStartPath, Stream: gjson.GetBytes(body, "stream").Bool(), Duration: time.Since(start), Usage: usage}
	if id, ok := payload["id"].(string); ok {
		result.ResponseID = id
	}
	if result.Stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		text := prismOutputText(payload)
		if text != "" {
			delta, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": text})
			_, _ = c.Writer.WriteString("event: response.output_text.delta\ndata: " + string(delta) + "\n\n")
		}
		completed, _ := json.Marshal(map[string]any{"type": "response.completed", "response": payload})
		_, _ = c.Writer.WriteString("event: response.completed\ndata: " + string(completed) + "\n\n")
		c.Writer.Flush()
	} else {
		c.Data(http.StatusOK, "application/json", payloadRaw)
	}
	_ = status
	return result, nil
}

// executePrismRequest owns the start/status polling shared by text and image
// routes. It never retries through another upstream protocol.
func (s *OpenAIGatewayService) executePrismRequest(requestCtx context.Context, account *Account, requestBody []byte, model, nonce string, c *gin.Context) (map[string]any, int, error) {
	conversationID := strings.TrimSpace(gjson.GetBytes(requestBody, "conversationId").String())
	if conversationID == "" {
		conversationID = prismConversationID(account)
	}
	startRaw, status, err := s.prismCall(requestCtx, account, prismStartPath, requestBody)
	if err != nil {
		setOpsUpstreamError(c, status, "Prism upstream request failed", "")
		recordPrismUpstreamError(c, account, status, err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"type": "server_error", "code": "prism_upstream_error", "message": err.Error()}})
		return nil, status, err
	}
	var current map[string]any
	if err := json.Unmarshal(startRaw, &current); err != nil {
		return nil, status, err
	}
	deadline := time.Now().Add(5 * time.Minute)
	for current["status"] == "started" || current["status"] == "pending" {
		if time.Now().After(deadline) {
			prismStopBody, _ := json.Marshal(map[string]any{"request_id": current["request_id"], "turn_state": current["turn_state"], "conversation_id": conversationID})
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _, _ = s.prismCall(stopCtx, account, prismStopPath, prismStopBody)
			cancel()
			err := fmt.Errorf("prism task timed out")
			recordPrismUpstreamError(c, account, http.StatusGatewayTimeout, err.Error())
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": gin.H{"type": "server_error", "code": "prism_timeout", "message": err.Error()}})
			return nil, status, err
		}
		requestID, _ := current["request_id"].(string)
		turnState := current["turn_state"]
		pollBody, _ := json.Marshal(map[string]any{"request_id": requestID, "turn_state": turnState})
		timer := time.NewTimer(time.Second)
		select {
		case <-requestCtx.Done():
			timer.Stop()
			stopBody, _ := json.Marshal(map[string]any{"request_id": current["request_id"], "turn_state": current["turn_state"], "conversation_id": conversationID})
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _, _ = s.prismCall(stopCtx, account, prismStopPath, stopBody)
			cancel()
			return nil, status, requestCtx.Err()
		case <-timer.C:
		}
		pollRaw, _, pollErr := s.prismCall(requestCtx, account, prismStatusPath, pollBody)
		if pollErr != nil {
			setOpsUpstreamError(c, status, "Prism status polling failed", "")
			recordPrismUpstreamError(c, account, status, pollErr.Error())
			return nil, status, pollErr
		}
		if err := json.Unmarshal(pollRaw, &current); err != nil {
			return nil, status, err
		}
	}
	payload, err := prismEnvelopePayload(prismMustJSON(current))
	if err != nil {
		recordPrismUpstreamError(c, account, http.StatusBadGateway, err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"type": "server_error", "code": "prism_task_error", "message": err.Error()}})
		return nil, status, err
	}
	payload["model"] = model
	payload["status"] = "completed"
	prismNormalizePayload(payload, model, nonce)
	return payload, status, nil
}

func recordPrismUpstreamError(c *gin.Context, account *Account, status int, message string) {
	if c == nil || account == nil {
		return
	}
	kind := "http_error"
	if status == 0 {
		kind = "request_error"
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
		ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
		UpstreamStatusCode: status, UpstreamURL: prismOrigin + prismStartPath,
		Kind: kind, Message: sanitizeUpstreamErrorMessage(message),
	})
}

// forwardPrismImages follows the BPS image contract: build a Responses
// image_generation tool request, execute it through Prism, then translate the
// completed image_generation_call results back to the Images API shape.
func (s *OpenAIGatewayService) forwardPrismImages(ctx context.Context, c *gin.Context, account *Account, parsed *OpenAIImagesRequest, requestModel string) (*OpenAIForwardResult, error) {
	start := time.Now()
	model := strings.TrimSpace(requestModel)
	if model == "" {
		model = parsed.Model
	}
	body, err := buildOpenAIImagesResponsesRequest(parsed, model)
	if err != nil {
		return nil, err
	}
	requestBody, prismModel, nonce, err := prismRequestBodyMode(account, body, true)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "code": "prism_configuration_error", "message": err.Error()}})
		return nil, err
	}
	SetActualOpenAIUpstreamEndpoint(c, prismStartPath)
	SetOpsUpstreamModel(c, prismModel)
	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()
	requestCtx := WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamProfile(upstreamCtx, HTTPUpstreamProfilePrism))
	upstreamStart := time.Now()
	payload, _, err := s.executePrismRequest(requestCtx, account, requestBody, prismModel, nonce, c)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
	if err != nil {
		return nil, err
	}
	completed, _ := json.Marshal(map[string]any{"type": "response.completed", "response": payload})
	images, _, usageRaw, _, _, err := collectOpenAIImagesFromResponsesBody(completed)
	if err != nil {
		return nil, err
	}
	if len(images) == 0 {
		upErr := &OpenAIImagesUpstreamError{StatusCode: http.StatusBadGateway, ErrorType: "upstream_error", Code: "image_generation_unavailable", Message: "Prism did not return an image output"}
		writeOpenAIImagesUpstreamErrorResponse(c, upErr)
		return nil, upErr
	}
	data := make([]map[string]any, 0, len(images))
	for _, image := range images {
		item := map[string]any{"b64_json": image.Result}
		if image.RevisedPrompt != "" {
			item["revised_prompt"] = image.RevisedPrompt
		}
		if strings.EqualFold(parsed.ResponseFormat, "url") {
			item["url"] = "data:" + openAIImageOutputMIMEType(image.OutputFormat) + ";base64," + image.Result
		}
		data = append(data, item)
	}
	var usage OpenAIUsage
	if len(usageRaw) > 0 {
		usage, _ = extractOpenAIUsageFromJSONBytes(usageRaw)
	}
	response := map[string]any{"created": time.Now().Unix(), "data": data}
	if parsed.Stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		for _, item := range data {
			event := map[string]any{"type": "image_generation.completed", "model": model}
			for key, value := range item {
				event[key] = value
			}
			raw, _ := json.Marshal(event)
			_, _ = c.Writer.WriteString("data: " + string(raw) + "\n\n")
		}
		_, _ = c.Writer.WriteString("data: [DONE]\n\n")
		if flusher, ok := c.Writer.(http.Flusher); ok { flusher.Flush() }
	} else {
		c.JSON(http.StatusOK, response)
	}
	return &OpenAIForwardResult{Model: model, UpstreamModel: prismModel, UpstreamEndpoint: prismStartPath, Stream: parsed.Stream, Duration: time.Since(start), Usage: usage, ImageCount: len(data), ImageSize: parsed.SizeTier, ImageInputSize: parsed.Size}, nil
}

func prismMustJSON(value map[string]any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}
