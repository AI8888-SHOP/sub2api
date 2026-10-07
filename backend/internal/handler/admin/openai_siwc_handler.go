package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/pkg/siwc"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *OpenAIOAuthHandler) StartSiwc(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var req struct {
		AccountID *int64                  `json:"account_id"`
		ProxyID   *int64                  `json:"proxy_id"`
		Login     *service.SiwcLoginInput `json:"login"`
	}
	if c.ShouldBindJSON(&req) != nil {
		response.BadRequest(c, "invalid SIWC login request")
		return
	}
	var auth *siwc.Authorization
	var err error
	if req.AccountID != nil {
		account, readErr := h.adminService.GetAccount(c.Request.Context(), *req.AccountID)
		if readErr != nil {
			response.ErrorFrom(c, readErr)
			return
		}
		auth, err = h.openaiOAuthService.GenerateSiwcReconnect(c.Request.Context(), account, req.Login)
	} else {
		auth, err = h.openaiOAuthService.GenerateSiwcAuth(c.Request.Context(), req.ProxyID, req.Login)
	}
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, auth)
}
func (h *OpenAIOAuthHandler) SiwcStatus(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	status, err := h.openaiOAuthService.SiwcLoginStatus(c.Param("session"))
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, status)
}
func (h *OpenAIOAuthHandler) CancelSiwc(c *gin.Context) {
	h.openaiOAuthService.CancelSiwcLogin(c.Param("session"))
	response.Success(c, gin.H{"cancelled": true})
}
func (h *OpenAIOAuthHandler) ExchangeSiwc(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var req struct {
		Session  string `json:"session_id"`
		Callback string `json:"callback_url"`
	}
	if c.ShouldBindJSON(&req) != nil || req.Session == "" || req.Callback == "" {
		response.BadRequest(c, "full SIWC callback URL is required")
		return
	}
	info, err := h.openaiOAuthService.ExchangeSiwcCallback(c.Request.Context(), req.Session, req.Callback)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, h.openaiOAuthService.BuildAccountCredentials(info))
}
