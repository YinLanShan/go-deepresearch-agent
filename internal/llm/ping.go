package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/fault"
)

// DefaultPingTimeout 是配置连通性探测的默认超时。
//
// 探测只发一个极短的请求，因此超时远小于正常推理。
const DefaultPingTimeout = 10 * time.Second

// pingPrompt 是探测用的最小请求，尽量少消耗 token。
const pingPrompt = "ping"

// Ping 用给定配置真实调用一次模型，验证密钥与地址是否可用。
//
// 用途：保存配置前先探测，避免"保存成功但一用就报错"。
//
// 语义区分：
//   - mock 类提供方不发起网络请求，直接视为可用（本地联调不应被阻塞）；
//   - 鉴权失败（401/403/invalid api key）→ invalid_input，提示用户检查密钥；
//   - 其余失败（网络、超时、服务异常）→ model_unavailable，提示检查地址/网络。
func Ping(ctx context.Context, cfg Config) error {
	cfg = cfg.WithDefaults()

	// 本地 mock 提供方不涉及网络，无需探测。
	if strings.HasPrefix(cfg.Provider, "mock") {
		return nil
	}

	timeout := cfg.PingTimeout
	if timeout <= 0 {
		timeout = DefaultPingTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	m, err := New(ctx, cfg)
	if err != nil {
		return err
	}

	// 探测不带工具，只要求模型能正常回包。
	if _, err := m.Generate(ctx, []*schema.Message{schema.UserMessage(pingPrompt)}); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fault.E(fault.CodeModelUnavailable, fault.LayerInfrastructure,
				fmt.Sprintf("model did not respond within %s", timeout)).
				Severity(fault.SeverityError).
				Source("llm.Ping").
				Wrap(err)
		}
		if isAuthError(err) {
			return fault.E(fault.CodeInvalidInput, fault.LayerInfrastructure,
				"invalid api key or unauthorized").
				Severity(fault.SeverityError).
				Source("llm.Ping").
				Wrap(err)
		}
		if isModelMissing(err) {
			return fault.E(fault.CodeInvalidInput, fault.LayerInfrastructure,
				"model name is not available for this endpoint").
				Severity(fault.SeverityError).
				Source("llm.Ping").
				Wrap(err)
		}
		return fault.E(fault.CodeModelUnavailable, fault.LayerInfrastructure,
			"failed to reach model service").
			Severity(fault.SeverityError).
			Source("llm.Ping").
			Wrap(err)
	}
	return nil
}

// isAuthError 判断错误是否为鉴权失败。
func isAuthError(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{"401", "403", "unauthorized", "invalid api key",
		"invalid_api_key", "api key", "authentication", "permission denied"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// isModelMissing 判断错误是否为模型名不可用。
func isModelMissing(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{"404", "model not found", "does not exist",
		"no such model", "unknown model"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}
