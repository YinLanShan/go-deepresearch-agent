// Package service 的 ConfigService 提供运行时模型配置的读取与热切换。
//
// 对应前端「API 配置」界面：用户可以在此查看/修改模型提供方、接口地址、
// 模型名称与密钥，保存后立即生效，无需重启服务。
package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/fault"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/llm"
)

// ModelConfigView 是模型配置的对外视图，密钥只暴露脱敏提示。
type ModelConfigView struct {
	Provider   string `json:"provider"`
	BaseURL    string `json:"base_url"`
	ModelName  string `json:"model_name"`
	APIKeySet  bool   `json:"api_key_set"`
	APIKeyHint string `json:"api_key_hint"`
}

// ModelConfigUpdate 是更新模型配置的入参。
type ModelConfigUpdate struct {
	Provider  string `json:"provider"`
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key"`
	ModelName string `json:"model_name"`
}

// ConfigService 提供运行时模型配置的读取与更新。
type ConfigService struct {
	model *llm.Swappable
}

// NewConfigService 构造 ConfigService。
func NewConfigService(m *llm.Swappable) *ConfigService {
	return &ConfigService{model: m}
}

// Get 返回当前生效的模型配置视图。
func (s *ConfigService) Get() ModelConfigView {
	cfg := s.model.Config().WithDefaults()
	return ModelConfigView{
		Provider:   cfg.Provider,
		BaseURL:    cfg.BaseURL,
		ModelName:  cfg.ModelName,
		APIKeySet:  cfg.APIKey != "",
		APIKeyHint: llm.MaskAPIKey(cfg.APIKey),
	}
}

// isSupportedProvider 判断模型提供方是否受支持。
//
//   - mock：进程内确定性模型，回显用户消息；
//   - mock-tool：进程内确定性模型，会发起一次 web_search 工具调用（用于验证 ReAct + 审批）；
//   - openai：OpenAI 兼容接口。
func isSupportedProvider(provider string) bool {
	switch provider {
	case "mock", "mock-tool", "openai":
		return true
	default:
		return false
	}
}

// Update 校验并热切换模型配置。
//
// APIKey 留空时沿用当前密钥，避免前端每次都回传脱敏值导致密钥被覆盖。
//
// 保存前会先做一次连通性探测（Ping）：失败则不落库、不热切换，
// 避免"配置保存成功但一用就报错"。mock 提供方不涉及网络，直接跳过探测。
func (s *ConfigService) Update(ctx context.Context, in ModelConfigUpdate) error {
	next, err := s.resolve(ctx, in)
	if err != nil {
		return err
	}

	// 探测失败即拒绝保存。
	if err := llm.Ping(ctx, next); err != nil {
		return err
	}

	if err := s.model.Swap(ctx, next); err != nil {
		return fault.Wrap(fault.CodeModelUnavailable, fault.LayerRuntime,
			"apply model config failed", err)
	}
	return nil
}

// Verify 只做连通性探测，不保存也不热切换。
//
// 供前端在"确认"前先行试探；与 Update 使用同一套解析规则，
// 因此探测结果能真实代表保存后的行为。
func (s *ConfigService) Verify(ctx context.Context, in ModelConfigUpdate) error {
	next, err := s.resolve(ctx, in)
	if err != nil {
		return err
	}
	return llm.Ping(ctx, next)
}

// resolve 把前端提交的增量配置解析成完整的模型配置。
//
// 规则：
//   - APIKey 留空 → 沿用当前密钥；
//   - BaseURL / ModelName 留空且提供方未变 → 沿用当前值
//     （避免只改密钥却把自定义地址/模型名清掉）；
//   - 提供方切换 → 不使用旧值，交给 WithDefaults 填充新提供方默认值
//     （否则切到 openai 仍会带着旧模型名，导致接口拒绝请求）。
func (s *ConfigService) resolve(_ context.Context, in ModelConfigUpdate) (llm.Config, error) {
	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		return llm.Config{}, fault.New(fault.CodeInvalidInput, fault.LayerRuntime,
			"provider is required")
	}
	if !isSupportedProvider(provider) {
		return llm.Config{}, fault.New(fault.CodeInvalidInput, fault.LayerRuntime,
			fmt.Sprintf("unsupported provider %q", provider))
	}

	cur := s.model.Config()
	apiKey := strings.TrimSpace(in.APIKey)
	if apiKey == "" {
		apiKey = cur.APIKey
	}
	if provider == "openai" && apiKey == "" {
		return llm.Config{}, fault.New(fault.CodeInvalidInput, fault.LayerRuntime,
			"api_key is required for openai provider")
	}

	baseURL := strings.TrimSpace(in.BaseURL)
	modelName := strings.TrimSpace(in.ModelName)
	if provider == cur.Provider {
		if baseURL == "" {
			baseURL = cur.BaseURL
		}
		if modelName == "" {
			modelName = cur.ModelName
		}
	}

	// 配置不全时回退到默认值，避免因 BaseURL / 模型名为空导致模型不可用。
	return llm.Config{
		Provider:  provider,
		BaseURL:   baseURL,
		APIKey:    apiKey,
		ModelName: modelName,
	}.WithDefaults(), nil
}
