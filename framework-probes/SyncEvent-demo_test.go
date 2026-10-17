package framework_probes

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Consumer 是支持同步确认的订阅者示例。
//
// 事件在独立 goroutine 中串行处理；同步投递时，处理完成后回执，
// 再等待发布方确认（等待放在另一个 goroutine，避免阻塞事件循环）。
type Consumer struct {
	events chan delivery
	stop   chan struct{}
	done   chan struct{}
	work   time.Duration
}

type delivery struct {
	env EventEnvelope
	h   *Handshake // nil 表示异步投递
}

// NewConsumer 构造一个 Consumer，work 为单次处理的模拟耗时。
func NewConsumer(work time.Duration) *Consumer {
	return &Consumer{
		events: make(chan delivery, 16), // 带缓冲，避免"队列满"抖动
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		work:   work,
	}
}

// Start 启动事件循环。
func (c *Consumer) Start() { go c.run() }

// Stop 停止事件循环并等待退出，避免 goroutine 泄漏。
func (c *Consumer) Stop() {
	close(c.stop)
	<-c.done
}

// Send 异步投递。
func (c *Consumer) Send(_ context.Context, e EventEnvelope) error {
	return c.enqueue(delivery{env: e})
}

// SendSync 同步投递：入队后立即返回，处理完成后由事件循环回执。
func (c *Consumer) SendSync(_ context.Context, e EventEnvelope, h *Handshake) error {
	return c.enqueue(delivery{env: e, h: h})
}

func (c *Consumer) enqueue(d delivery) error {
	select {
	case c.events <- d:
		return nil
	case <-c.stop:
		return errors.New("consumer: stopped")
	}
}

func (c *Consumer) run() {
	defer close(c.done)
	for {
		select {
		case d := <-c.events:
			c.handle(d)
		case <-c.stop:
			return
		}
	}
}

func (c *Consumer) handle(d delivery) {
	// 串行处理：保持对 RunTask 状态变更的顺序性。
	err := c.process(d.env)
	if d.h == nil {
		return
	}
	// 第 2 次握手：回执。
	d.h.Reply(err)
	// 第 3 次握手：等待发布方确认。放到独立 goroutine，不阻塞事件循环；
	// 且带超时，避免发布方异常消失时永久挂起。
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = d.h.Confirmed(ctx)
	}()
}

func (c *Consumer) process(e EventEnvelope) error {
	switch e.Data.(type) {
	case RunTaskDeletedEvent:
		time.Sleep(c.work)
		return nil
	default:
		return nil
	}
}

// TestPublishSyncSuccess 同步投递：发布方等到接收方处理完成后才返回。
func TestPublishSyncSuccess(t *testing.T) {
	bus := NewBus()
	c := NewConsumer(20 * time.Millisecond)
	c.Start()
	defer c.Stop()
	bus.Subscribe(EventRunTaskDeleted, c)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := bus.PublishSync(ctx, EventEnvelope{Data: RunTaskDeletedEvent{RunTaskID: "r1"}}); err != nil {
		t.Fatalf("PublishSync: %v", err)
	}
}

// TestPublishSyncTimeout 超时：必须返回 DeadlineExceeded，不能误报成功。
func TestPublishSyncTimeout(t *testing.T) {
	bus := NewBus()
	c := NewConsumer(500 * time.Millisecond)
	c.Start()
	defer c.Stop()
	bus.Subscribe(EventRunTaskDeleted, c)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	err := bus.PublishSync(ctx, EventEnvelope{Data: RunTaskDeletedEvent{RunTaskID: "r1"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

// TestPublishAsyncDoesNotWait 异步投递不应被接收方处理时长拖住。
func TestPublishAsyncDoesNotWait(t *testing.T) {
	bus := NewBus()
	c := NewConsumer(500 * time.Millisecond)
	c.Start()
	defer c.Stop()
	bus.Subscribe(EventRunTaskDeleted, c)

	start := time.Now()
	if err := bus.Publish(context.Background(), EventEnvelope{Data: RunTaskDeletedEvent{RunTaskID: "r1"}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("异步投递不应等待处理，耗时 %v", elapsed)
	}
}

// TestPublishSyncConcurrent 并发同步投递：验证无 panic、无丢失、无死锁。
func TestPublishSyncConcurrent(t *testing.T) {
	bus := NewBus()
	c := NewConsumer(time.Millisecond)
	c.Start()
	defer c.Stop()
	bus.Subscribe(EventRunTaskDeleted, c)

	const n = 32
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			errCh <- bus.PublishSync(ctx, EventEnvelope{Data: RunTaskDeletedEvent{RunTaskID: "r"}})
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("PublishSync: %v", err)
		}
	}
}

// TestPublishSyncUnsupportedSubscriber 订阅者不支持同步时，应显式报错而非静默降级。
func TestPublishSyncUnsupportedSubscriber(t *testing.T) {
	bus := NewBus()
	bus.Subscribe(EventRunTaskDeleted, asyncOnlySubscriber{})

	err := bus.PublishSync(context.Background(), EventEnvelope{Data: RunTaskDeletedEvent{RunTaskID: "r1"}})
	if err == nil {
		t.Fatal("want error for subscriber without sync support")
	}
}

type asyncOnlySubscriber struct{}

func (asyncOnlySubscriber) Send(context.Context, EventEnvelope) error { return nil }
