// Package llm 的 Swappable 提供可在运行时热切换的模型实例。
//
// 它本身实现 model.BaseChatModel，内部委派给当前真实模型；
// 因此 Runtime、SupervisorLoop、ChatService 只需持有一个 Swappable，
// 由 API 配置界面调用 Swap 即可在不重建这些组件的前提下切换模型。
package llm

import (
	"context"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Swappable 是可热切换的 ChatModel 持有者。
type Swappable struct {
	mu    sync.RWMutex
	inner ChatModel
	cfg   Config
}

// 编译期断言：Swappable 实现 eino 的基础模型接口。
var _ model.BaseChatModel = (*Swappable)(nil)

// NewSwappable 根据初始配置构造一个可热切换的模型。
func NewSwappable(ctx context.Context, cfg Config) (*Swappable, error) {
	inner, err := New(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Swappable{inner: inner, cfg: cfg}, nil
}

// Generate 委派给当前模型进行非流式调用。
func (s *Swappable) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return s.current().Generate(ctx, input, opts...)
}

// Stream 委派给当前模型进行流式调用。
func (s *Swappable) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return s.current().Stream(ctx, input, opts...)
}

func (s *Swappable) current() ChatModel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inner
}

// Config 返回当前生效的模型配置。
func (s *Swappable) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Swap 用新配置构建模型并原子替换；构建失败时保持原模型不变。
func (s *Swappable) Swap(ctx context.Context, cfg Config) error {
	inner, err := New(ctx, cfg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inner = inner
	s.cfg = cfg
	return nil
}

// MaskAPIKey 返回用于展示的脱敏密钥（保留前 3 与后 4 位）。
func MaskAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	r := []rune(key)
	if len(r) <= 8 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:3]) + strings.Repeat("*", len(r)-7) + string(r[len(r)-4:])
}
