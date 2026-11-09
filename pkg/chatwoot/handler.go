package chatwoot

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
)

const maxWebhookBody = 1 << 20

// RegisterRoutes adds the integration endpoints. auth must authenticate the
// instance token and store the instance under the "instance" context key.
//
//	POST /chatwoot/set                   configure the instance (instance token)
//	GET  /chatwoot/find                  read the configuration (instance token)
//	POST /chatwoot/webhook/:instanceId   called by Chatwoot (HMAC signature)
func RegisterRoutes(r gin.IRouter, auth gin.HandlerFunc, s *Service) {
	g := r.Group("/chatwoot")
	g.POST("/set", auth, s.handleSet)
	g.GET("/find", auth, s.handleFind)
	g.POST("/webhook/:instanceId", s.handleWebhook)
}

type configResponse struct {
	Config
	WebhookURL string `json:"webhookUrl"`
}

// newConfigResponse hides the Chatwoot token, which only needs to be written.
func newConfigResponse(cfg *Config, webhookURL string) configResponse {
	out := configResponse{Config: *cfg, WebhookURL: webhookURL}
	out.Token = maskToken(cfg.Token)
	return out
}

func maskToken(token string) string {
	if len(token) <= 4 {
		return ""
	}
	return token[:4] + "…"
}

func instanceFrom(ctx *gin.Context) (*instance_model.Instance, bool) {
	inst, ok := ctx.MustGet("instance").(*instance_model.Instance)
	if !ok {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "instance not found"})
	}
	return inst, ok
}

func (s *Service) handleSet(ctx *gin.Context) {
	inst, ok := instanceFrom(ctx)
	if !ok {
		return
	}
	var req SetRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	cfg, err := s.SetConfig(ctx.Request.Context(), inst, req)
	if err != nil {
		status := http.StatusBadGateway // Chatwoot could not be reached or refused the request
		if errors.Is(err, ErrInvalidConfig) {
			status = http.StatusBadRequest
		}
		ctx.JSON(status, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"message": "success", "data": newConfigResponse(cfg, s.WebhookURL(inst.Id))})
}

func (s *Service) handleFind(ctx *gin.Context) {
	inst, ok := instanceFrom(ctx)
	if !ok {
		return
	}
	cfg, err := s.GetConfig(inst.Id)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if cfg == nil {
		cfg = &Config{InstanceID: inst.Id}
	}
	ctx.JSON(http.StatusOK, gin.H{"message": "success", "data": newConfigResponse(cfg, s.WebhookURL(inst.Id))})
}

func (s *Service) handleWebhook(ctx *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(ctx.Request.Body, maxWebhookBody))
	if err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "could not read body"})
		return
	}
	err = s.HandleWebhook(ctx.Param("instanceId"), ctx.Request.Header, body)
	switch {
	case err == nil:
		ctx.JSON(http.StatusOK, gin.H{"message": "ok"})
	case errors.Is(err, ErrInvalidSignature):
		ctx.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
	case errors.Is(err, ErrNotConfigured):
		ctx.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	default:
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	}
}
