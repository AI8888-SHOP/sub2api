package service

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/siwc"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func (s *OpenAIGatewayService) forwardSiwc(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	reject := func(err error) (*OpenAIForwardResult, error) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": err.Error()}})
		return nil, err
	}
	if !account.HasSiwcSharing() {
		return reject(errors.New("SIWC sharing consent is required"))
	}
	if suffix := openAIResponsesRequestPathSuffix(c); suffix != "" {
		return reject(errors.New("SIWC only supports /v1/responses"))
	}
	model := gjson.GetBytes(body, "model").String()
	stream := gjson.GetBytes(body, "stream").Bool()
	mapped := account.GetMappedModel(model)
	var err error
	body, err = sjson.SetBytes(body, "model", mapped)
	if err != nil {
		return reject(err)
	}
	body, err = siwc.NormalizePayload(body)
	if err != nil {
		return reject(err)
	}
	return s.forwardOpenAIPassthrough(ctx, c, account, body, body, model, false, nil, stream, time.Now())
}
