package chatwoot

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
)

// testRouter mounts the routes with an auth middleware that accepts the
// instance token, like the server's.
func testRouter(h *harness) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	auth := func(ctx *gin.Context) {
		if ctx.GetHeader("apikey") != "instance-token" {
			ctx.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "not authorized"})
			return
		}
		ctx.Set("instance", h.inst)
		ctx.Next()
	}
	RegisterRoutes(r, auth, h.svc)
	return r
}

func doRequest(r http.Handler, method, path string, body []byte, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	for k, v := range header {
		req.Header[k] = v
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestSetAndFindRoutes(t *testing.T) {
	h := newHarness(t)
	r := testRouter(h)
	auth := http.Header{"Apikey": {"instance-token"}, "Content-Type": {"application/json"}}

	t.Run("requires instance token", func(t *testing.T) {
		if w := doRequest(r, "GET", "/chatwoot/find", nil, nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("status %d", w.Code)
		}
		if w := doRequest(r, "POST", "/chatwoot/set", []byte(`{}`), nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("status %d", w.Code)
		}
	})

	t.Run("find hides token and secret", func(t *testing.T) {
		w := doRequest(r, "GET", "/chatwoot/find", nil, auth)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
		body := w.Body.String()
		if strings.Contains(body, testToken) || strings.Contains(body, testSecret) {
			t.Fatalf("response leaks credentials: %s", body)
		}
		var resp struct {
			Data struct {
				Enabled    bool   `json:"enabled"`
				InboxID    int    `json:"inboxId"`
				Token      string `json:"token"`
				WebhookURL string `json:"webhookUrl"`
			} `json:"data"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		if !resp.Data.Enabled || resp.Data.InboxID == 0 || resp.Data.Token != "cw-a…" {
			t.Errorf("unexpected data %+v", resp.Data)
		}
		if resp.Data.WebhookURL != webhookBase+"/chatwoot/webhook/inst-1" {
			t.Errorf("webhook URL %q", resp.Data.WebhookURL)
		}
	})

	t.Run("set validates input", func(t *testing.T) {
		w := doRequest(r, "POST", "/chatwoot/set", []byte(`{"enabled":true,"url":"","token":""}`), auth)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
		if w := doRequest(r, "POST", "/chatwoot/set", []byte(`not json`), auth); w.Code != http.StatusBadRequest {
			t.Fatalf("status %d", w.Code)
		}
	})

	t.Run("set reports Chatwoot errors as bad gateway", func(t *testing.T) {
		w := doRequest(r, "POST", "/chatwoot/set", []byte(`{"token":"wrong"}`), auth)
		if w.Code != http.StatusBadGateway {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
	})

	t.Run("set updates options", func(t *testing.T) {
		w := doRequest(r, "POST", "/chatwoot/set", []byte(`{"token":"`+testToken+`","conversationPending":true}`), auth)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
		if cfg, _ := h.repo.GetConfig("inst-1"); !cfg.ConversationPending {
			t.Fatal("option not saved")
		}
	})
}

func TestWebhookRoute(t *testing.T) {
	h := newHarness(t)
	r := testRouter(h)
	body, _ := json.Marshal(agentMessage(900, 1, "oi"))

	if w := doRequest(r, "POST", "/chatwoot/webhook/inst-1", body, signedHeaders("wrong", body, time.Now())); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: status %d", w.Code)
	}
	if w := doRequest(r, "POST", "/chatwoot/webhook/unknown", body, signedHeaders(testSecret, body, time.Now())); w.Code != http.StatusNotFound {
		t.Fatalf("unknown instance: status %d", w.Code)
	}
	if w := doRequest(r, "POST", "/chatwoot/webhook/inst-1", body, signedHeaders(testSecret, body, time.Now())); w.Code != http.StatusOK {
		t.Fatalf("valid webhook: status %d: %s", w.Code, w.Body)
	}
	h.svc.Wait()
	if len(h.wa.sentMessages()) != 1 {
		t.Fatal("valid webhook did not send the message")
	}
}

func TestInstanceMissingFromContext(t *testing.T) {
	h := newHarness(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterRoutes(r, func(ctx *gin.Context) { ctx.Set("instance", "not an instance"); ctx.Next() }, h.svc)
	if w := doRequest(r, "GET", "/chatwoot/find", nil, nil); w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", w.Code)
	}
	_ = instance_model.Instance{}
}
