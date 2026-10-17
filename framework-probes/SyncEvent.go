package framework_probes

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// 事件类型常量（控制类事件）。
const (
	// EventTaskEnabled 表示 Task 被启用，Runtime 应创建/恢复对应的 RunTask。
	EventTaskEnabled = "task_enabled"
	// EventTaskDisabled 表示 Task 被禁用，Runtime 应暂停对应的 RunTask。
	EventTaskDisabled = "task_disabled"

	EventRunTaskDeleted = "run_task_deleted"
)

// Event 描述事件的业务类型及数据。
type Event interface {
	EventType() string
}

// EventResponse 是接收方完成处理后的回执。
type EventResponse struct {
	ResponseWithError error
}

// EventMeta 提供统一的事件元数据。
//
// 【改动】这里只保留可序列化字段。原设计把 ctx / chan 放进 EventMeta，有三个问题：
//  1. 事件无法序列化 / 持久化（chan、context.Context 都不能编码）；
//  2. 同一事件投递给多个订阅者时会共享同一组通道，"谁发送、谁关闭"无法界定；
//  3. Go 明确不建议把 context.Context 存进结构体（应随调用传递）。
//
// 结论：同步握手是"这一次投递"的属性，不是"事件本身"的属性，
// 改由投递参数传递（见 Handshake 与 Bus.PublishSync）。
type EventMeta struct {
	EventID    string
	Source     string
	TaskID     string
	RunTaskID  string
	OccurredAt time.Time
}

// EventEnvelope 是携带元数据的事件。
type EventEnvelope struct {
	Meta EventMeta
	Data Event
}

// EventSender 接收事件（订阅者）。
// Send 返回 nil 只代表事件已被"接纳"（例如入队），不代表接收方已处理完成。
type EventSender interface {
	Send(ctx context.Context, e EventEnvelope) error
}

// SyncEventSender 是可同步确认的订阅者。
//
// SendSync 只负责把事件交给处理方（入队），必须立即返回；
// 处理完成由处理方调用 h.Reply(err) 回执，真正的等待由 Bus 通过 h.Wait 完成。
type SyncEventSender interface {
	SendSync(ctx context.Context, e EventEnvelope, h *Handshake) error
}

// Handshake 承载一次同步投递的三次握手。
//
//	发布方                            接收方
//	  |--- 事件 + Handshake ----------->|  1) 投递
//	  |<-- h.Reply(err) ----------------|  2) 处理完成，回执
//	  |--- h.Confirm() ---------------->|  3) 发布方确认（接收方据此释放）
//
// 两个通道都带 1 个缓冲，因此：
//   - 发送方永不阻塞（无需 select+default 的"伪非阻塞"写法）；
//   - 双方都不需要 close 通道，从根本上避免
//     "send on closed channel" 与 "close 时仍有发送方阻塞" 两类竞争。
type Handshake struct {
	reply   chan EventResponse
	confirm chan struct{}
	replyOn sync.Once
	confOn  sync.Once
}

// NewHandshake 由发布方创建一次握手。
func NewHandshake() *Handshake {
	return &Handshake{
		reply:   make(chan EventResponse, 1),
		confirm: make(chan struct{}),
	}
}

// Reply 由接收方调用一次，提交处理结果；重复调用被忽略。
func (h *Handshake) Reply(err error) {
	h.replyOn.Do(func() { h.reply <- EventResponse{ResponseWithError: err} })
}

// Wait 由发布方等待回执。
//
// ctx 超时返回 ctx.Err()，绝不会把"没收到回执"当成成功
// （原实现中接收方超时关闭通道后，发布方会读到零值，从而把失败误报为成功）。
func (h *Handshake) Wait(ctx context.Context) (EventResponse, error) {
	select {
	case r := <-h.reply:
		return r, nil
	case <-ctx.Done():
		return EventResponse{}, ctx.Err()
	}
}

// Confirm 由发布方调用一次，结束本次握手。
// 无论等待成功还是超时都应调用，接收方才能解除等待并释放资源。
func (h *Handshake) Confirm() {
	h.confOn.Do(func() { close(h.confirm) })
}

// Confirmed 由接收方等待发布方确认。ctx 超时返回 ctx.Err()，避免无限期挂起。
func (h *Handshake) Confirmed(ctx context.Context) error {
	select {
	case <-h.confirm:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Bus 是进程内事件总线。
type Bus struct {
	mu   sync.RWMutex
	subs map[string][]EventSender
}

// NewBus 构造一个事件总线。
func NewBus() *Bus {
	return &Bus{subs: make(map[string][]EventSender)}
}

// Subscribe 订阅指定事件类型。
func (b *Bus) Subscribe(eventType string, s EventSender) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[eventType] = append(b.subs[eventType], s)
}

// Publish 异步投递：只保证事件被接纳，不等待处理完成。
func (b *Bus) Publish(ctx context.Context, e EventEnvelope) error {
	return b.dispatch(ctx, e, false)
}

// PublishSync 同步投递：等待所有订阅者处理完成后再返回。
//
// 订阅者必须实现 SyncEventSender；否则返回错误，
// 而不是悄悄退化成异步（否则调用方会误以为对方已处理完成）。
func (b *Bus) PublishSync(ctx context.Context, e EventEnvelope) error {
	return b.dispatch(ctx, e, true)
}

func (b *Bus) dispatch(ctx context.Context, e EventEnvelope, syncMode bool) error {
	if e.Data == nil {
		return nil
	}
	b.mu.RLock()
	subs := make([]EventSender, len(b.subs[e.Data.EventType()]))
	copy(subs, b.subs[e.Data.EventType()])
	b.mu.RUnlock()

	var errs []error
	for _, s := range subs {
		if !syncMode {
			if err := s.Send(ctx, e); err != nil {
				errs = append(errs, err)
			}
			continue
		}

		ss, ok := s.(SyncEventSender)
		if !ok {
			errs = append(errs, fmt.Errorf(
				"event: subscriber does not support sync delivery for %q", e.Data.EventType()))
			continue
		}

		h := NewHandshake()
		if err := ss.SendSync(ctx, e, h); err != nil {
			errs = append(errs, err)
			h.Confirm() // 投递失败也要确认，避免接收方（若已启动）悬挂
			continue
		}
		res, err := h.Wait(ctx)
		switch {
		case err != nil:
			errs = append(errs, err)
		case res.ResponseWithError != nil:
			errs = append(errs, res.ResponseWithError)
		}
		h.Confirm() // 无论成败都确认，接收方得以释放
	}
	return errors.Join(errs...)
}

// RunTaskDeletedEvent 表示 RunTask 已删除。
type RunTaskDeletedEvent struct {
	RunTaskID string
}

// EventType 实现 Event 接口。
func (RunTaskDeletedEvent) EventType() string { return EventRunTaskDeleted }
