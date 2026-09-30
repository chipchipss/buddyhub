// Package server: singleflight.go —— 按 key 合并并发的凭据续期。
//
// 为什么需要：Cline 与 AutoClaw 的 refresh_token 都是**一次性轮换**语义——
// 服务端每次刷新会换发新的 refresh_token，旧的那份随即作废。多个并发请求
// 同时发现 token 临期时，若各自去打一次续期，后到的那次就拿着已经作废的
// refresh_token → 401 → **用户被踢下线**。
//
// 这里把「同一 key 同时只跑一次，其余等结果」这件事做成通用原语，
// 两条通道共用一份实现（而不是各写一遍 map+channel）。
package server

import "sync"

// flightGroup 按 key 合并并发调用：同一 key 同时只有一个 fn 在跑，
// 其余调用等它结束后复用同一个结果。
type flightGroup[T any] struct {
	mu    sync.Mutex
	inFly map[string]*flightCall[T]
}

type flightCall[T any] struct {
	done chan struct{}
	val  T
	err  error
}

// Do 执行 fn；同 key 的并发调用会等待并复用首次调用的结果。
func (g *flightGroup[T]) Do(key string, fn func() (T, error)) (T, error) {
	g.mu.Lock()
	if g.inFly == nil {
		g.inFly = map[string]*flightCall[T]{}
	}
	if call, ok := g.inFly[key]; ok {
		g.mu.Unlock()
		<-call.done // 等首次调用结束
		return call.val, call.err
	}
	call := &flightCall[T]{done: make(chan struct{})}
	g.inFly[key] = call
	g.mu.Unlock()

	call.val, call.err = fn()
	close(call.done) // 先放行等待者，再摘表

	g.mu.Lock()
	delete(g.inFly, key)
	g.mu.Unlock()
	return call.val, call.err
}
