package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// forwardPrismChatCompletions and forwardPrismAnthropic are protocol adapters:
// Prism remains the only upstream, while clients keep their requested wire
// format. This is the same boundary used by the normal Responses converters.
func (s *OpenAIGatewayService) forwardPrismChatCompletions(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	var request apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("parse chat completions request for Prism: %w", err)
	}
	responses, err := apicompat.ChatCompletionsToResponses(&request)
	if err != nil {
		return nil, fmt.Errorf("convert chat completions request for Prism: %w", err)
	}
	responsesBody, err := json.Marshal(responses)
	if err != nil {
		return nil, fmt.Errorf("marshal Prism Responses request: %w", err)
	}
	result, payload, err := s.forwardPrismPayload(ctx, c, account, responsesBody)
	if err != nil {
		return nil, err
	}
	var response apicompat.ResponsesResponse
	if err := marshalMapInto(payload, &response); err != nil {
		return nil, err
	}
	converted := apicompat.ResponsesToChatCompletions(&response, request.Model)
	if gjson.GetBytes(body, "stream").Bool() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		data, _ := json.Marshal(converted)
		_, _ = c.Writer.WriteString("data: " + string(data) + "\n\n")
		_, _ = c.Writer.WriteString("data: [DONE]\n\n")
		if flusher, ok := c.Writer.(http.Flusher); ok {
			flusher.Flush()
		}
	} else {
		c.JSON(http.StatusOK, converted)
	}
	return result, nil
}

func (s *OpenAIGatewayService) forwardPrismAnthropic(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	var request apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("parse Anthropic request for Prism: %w", err)
	}
	responses, err := apicompat.AnthropicToResponses(&request)
	if err != nil {
		return nil, fmt.Errorf("convert Anthropic request for Prism: %w", err)
	}
	responsesBody, err := json.Marshal(responses)
	if err != nil {
		return nil, fmt.Errorf("marshal Prism Responses request: %w", err)
	}
	result, payload, err := s.forwardPrismPayload(ctx, c, account, responsesBody)
	if err != nil {
		return nil, err
	}
	var response apicompat.ResponsesResponse
	if err := marshalMapInto(payload, &response); err != nil {
		return nil, err
	}
	converted := apicompat.ResponsesToAnthropic(&response, request.Model)
	if request.Stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		// Prism has no token stream. Emit the completed Anthropic message as a
		// valid compatibility event sequence after the task finishes.
		c.Writer.WriteString("event: message_start\ndata: {}\n\n")
		data, _ := json.Marshal(converted)
		c.Writer.WriteString("event: message_stop\ndata: " + string(data) + "\n\n")
		if flusher, ok := c.Writer.(http.Flusher); ok {
			flusher.Flush()
		}
	} else {
		c.JSON(http.StatusOK, converted)
	}
	return result, nil
}

func marshalMapInto(value map[string]any, target any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func (s *OpenAIGatewayService) forwardPrismPayload(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, map[string]any, error) {
	requestBody, model, nonce, err := prismRequestBody(account, body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "code": "prism_configuration_error", "message": err.Error()}})
		return nil, nil, err
	}
	SetActualOpenAIUpstreamEndpoint(c, prismStartPath)
	SetOpsUpstreamModel(c, model)
	upstreamCtx, release := detachUpstreamContext(ctx)
	defer release()
	started := time.Now()
	payload, _, err := s.executePrismRequest(WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamProfile(upstreamCtx, HTTPUpstreamProfilePrism)), account, requestBody, model, nonce, c)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(started).Milliseconds())
	if err != nil {
		return nil, nil, err
	}
	result := &OpenAIForwardResult{Model: model, UpstreamModel: model, UpstreamEndpoint: prismStartPath, Stream: gjson.GetBytes(body, "stream").Bool(), Duration: time.Since(started)}
	raw, _ := json.Marshal(payload)
	result.Usage, _ = extractOpenAIUsageFromJSONBytes(raw)
	return result, payload, nil
}

func prismRequestModel(body []byte) string {
	return strings.TrimSpace(gjson.GetBytes(body, "model").String())
}
