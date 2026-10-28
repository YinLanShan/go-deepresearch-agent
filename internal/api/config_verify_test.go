package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"
)

// TestVerifyConfigMockProvider 验证 mock 提供方无需网络即可通过探测。
func TestVerifyConfigMockProvider(t *testing.T) {
	h := newHarness()
	defer h.stop(t)
	router := h.router(t)

	w := ut.PerformRequest(router, "POST", "/api/v1/config/verify",
		jsonBody(t, map[string]string{"provider": "mock"}), jsonHeader())
	resp := w.Result()
	if resp.StatusCode() != http.StatusOK {
		t.Fatalf("want 200, got %d, body=%s", resp.StatusCode(), resp.Body())
	}
	var body struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(resp.Body(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body.OK {
		t.Fatalf("want ok=true, got %s", resp.Body())
	}
}

// TestVerifyConfigRejectsUnknownProvider 验证非法提供方返回 400。
func TestVerifyConfigRejectsUnknownProvider(t *testing.T) {
	h := newHarness()
	defer h.stop(t)
	router := h.router(t)

	w := ut.PerformRequest(router, "POST", "/api/v1/config/verify",
		jsonBody(t, map[string]string{"provider": "nope"}), jsonHeader())
	if resp := w.Result(); resp.StatusCode() != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode())
	}
}

// TestVerifyConfigRejectsBadKey 验证密钥无效时探测失败（401 → 400）。
//
// 用本地 httptest 模拟 OpenAI 兼容端点返回 401，避免依赖真实网络。
func TestVerifyConfigRejectsBadKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer upstream.Close()

	h := newHarness()
	defer h.stop(t)
	router := h.router(t)

	w := ut.PerformRequest(router, "POST", "/api/v1/config/verify",
		jsonBody(t, map[string]string{
			"provider":   "openai",
			"base_url":   upstream.URL,
			"api_key":    "sk-invalid",
			"model_name": "any",
		}), jsonHeader())

	resp := w.Result()
	if resp.StatusCode() != http.StatusBadRequest {
		t.Fatalf("want 400, got %d, body=%s", resp.StatusCode(), resp.Body())
	}
	body := decodeErrorBody(t, resp.Body())
	if body.Error == "" {
		t.Fatalf("error message should not be empty: %s", resp.Body())
	}
}

// TestUpdateConfigRejectsBadKey 验证保存时同样会探测，失败则不落库。
func TestUpdateConfigRejectsBadKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer upstream.Close()

	h := newHarness()
	defer h.stop(t)
	router := h.router(t)

	w := ut.PerformRequest(router, "PUT", "/api/v1/config",
		jsonBody(t, map[string]string{
			"provider":   "openai",
			"base_url":   upstream.URL,
			"api_key":    "sk-invalid",
			"model_name": "any",
		}), jsonHeader())

	if resp := w.Result(); resp.StatusCode() != http.StatusBadRequest {
		t.Fatalf("want 400 (probe failed), got %d, body=%s", resp.StatusCode(), resp.Body())
	}

	// 配置应保持原样（仍为初始的 mock 提供方）。
	got := h.api.config.Get()
	if got.Provider == "openai" {
		t.Fatalf("config should not be applied when probe fails, got %+v", got)
	}
}
