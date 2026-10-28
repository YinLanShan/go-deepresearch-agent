package fault

import (
	"context"
	"errors"
)

// RetryOnce 执行一次操作，失败后重试一次。
//
// 约定（对应错误处理规范）：
//   - ErrNotFound 不重试：这是确定性结果，重试也不会变成"找得到"；
//   - ctx 已取消不重试：重试没有意义；
//   - 只重试一次：避免在内存存储场景下放大错误。
//
// 返回最后一次的错误（成功则为 nil）。
func RetryOnce(ctx context.Context, op func() error) error {
	if err := op(); err == nil {
		return nil
	} else if noRetry(ctx, err) {
		return err
	}

	if ctx != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	return op()
}

// noRetry 判断错误是否属于"重试没有意义"的确定性失败。
func noRetry(ctx context.Context, err error) bool {
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotAccepting) {
		return true
	}
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	return false
}

// 便于各层统一判断"实体已不存在"的哨兵错误。
//
// 存储层各自定义了 ErrNotFound（storage / researchstate），
// 这里提供统一入口避免调用方逐个 import。
var (
	// ErrNotFound 表示目标实体不存在（由调用方决定是否忽略）。
	ErrNotFound = errors.New("fault: not found")
	// ErrNotAccepting 表示目标当前不接受该操作（如提交门控已关闭）。
	ErrNotAccepting = errors.New("fault: not accepting")
)

// IsNotFound 判断错误（或其包装链）是否为"实体不存在"。
func IsNotFound(err error, sentinels ...error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotFound) {
		return true
	}
	for _, s := range sentinels {
		if s != nil && errors.Is(err, s) {
			return true
		}
	}
	return false
}
