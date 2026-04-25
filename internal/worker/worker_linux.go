//go:build linux

package worker

import (
	"fmt"
	"math/rand"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	vishnetns "github.com/vishvananda/netns"
	"surfshark-proxy/internal/discovery"
	nsmanager "surfshark-proxy/internal/netns"
	"surfshark-proxy/internal/router"
)

// Worker 表示一个活跃的 VPN 连接。
type Worker struct {
	mu          sync.RWMutex
	stopOnce    sync.Once
	stopErr     error
	processDone chan struct{}

	ID          string
	Index       int
	Server      discovery.Server
	State       router.WorkerState
	Namespace   *nsmanager.Namespace
	OvpnProcess *exec.Cmd
	ActiveConns int
	CreatedAt   time.Time
	LastUsed    time.Time

	effectiveMaxLifetime time.Duration
	rotationScheduledAt  time.Time

	processExited atomic.Bool
}

// NsHandle 返回 worker 所属命名空间句柄。
func (w *Worker) NsHandle() vishnetns.NsHandle {
	w.mu.RLock()
	defer w.mu.RUnlock()

	if w.Namespace == nil {
		return vishnetns.None()
	}

	return w.Namespace.Handle
}

// Info 返回路由层所需的只读 worker 视图。
func (w *Worker) Info() *router.WorkerInfo {
	w.mu.RLock()
	defer w.mu.RUnlock()

	return &router.WorkerInfo{
		ID:      w.ID,
		Country: w.Server.Country,
		State:   w.State,
	}
}

// IncrConns 记录新连接进入。
func (w *Worker) IncrConns() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.ActiveConns++
	w.LastUsed = time.Now()
	if w.State == router.WorkerIdle {
		w.State = router.WorkerReady
	}
}

// DecrConns 记录连接结束。
func (w *Worker) DecrConns() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.ActiveConns--
	if w.ActiveConns < 0 {
		w.ActiveConns = 0
	}
	w.LastUsed = time.Now()
	if w.ActiveConns == 0 && w.State == router.WorkerReady {
		w.State = router.WorkerIdle
	}
}

// IsIdle 判断当前 worker 是否无活跃连接。
func (w *Worker) IsIdle() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.ActiveConns == 0
}

// LastUsedAt 返回最近使用时间快照。
func (w *Worker) LastUsedAt() time.Time {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.LastUsed
}

// ProcessExited 返回 OpenVPN 进程是否已退出。
func (w *Worker) ProcessExited() bool {
	return w.processExited.Load()
}

func (w *Worker) markReady() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.State = router.WorkerReady
	w.LastUsed = time.Now()
}

// Age 返回 worker 已存活时长。
func (w *Worker) Age() time.Duration {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return time.Since(w.CreatedAt)
}

// markClosing 标记 worker 进入 draining 状态，停止接纳新请求。
// 只有 Ready/Idle 状态的 worker 会被标记，已经 Closing/Creating 的保持原状。
// 返回 true 表示本次调用真的发生了状态转换。
func (w *Worker) markClosing() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.State == router.WorkerReady || w.State == router.WorkerIdle {
		w.State = router.WorkerClosing
		return true
	}
	return false
}

// isClosingDrained 判断是否已进入 Closing 且所有连接都已排空，可以回收。
func (w *Worker) isClosingDrained() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.State == router.WorkerClosing && w.ActiveConns == 0
}

// markAwaitingReplacement 仅允许 Ready/Idle 进入 AwaitingReplacement。
func (w *Worker) markAwaitingReplacement() bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.State == router.WorkerReady || w.State == router.WorkerIdle {
		w.State = router.WorkerAwaitingReplacement
		w.rotationScheduledAt = time.Now()
		return true
	}

	return false
}

// isAwaitingReplacement 返回当前 worker 是否正在等待替换。
func (w *Worker) isAwaitingReplacement() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.State == router.WorkerAwaitingReplacement
}

// rotationScheduledSince 返回进入 AwaitingReplacement 的时间。
func (w *Worker) rotationScheduledSince() time.Time {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.rotationScheduledAt
}

// markClosingFromAwaiting 仅允许 AwaitingReplacement 进入 Closing。
func (w *Worker) markClosingFromAwaiting() bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.State == router.WorkerAwaitingReplacement {
		w.State = router.WorkerClosing
		return true
	}

	return false
}

// effectiveLifetime 返回 worker 的有效寿命；未设置时由调用方决定兜底值。
func (w *Worker) effectiveLifetime() time.Duration {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.effectiveMaxLifetime
}

// computeEffectiveMaxLifetime 对最大寿命应用单向向下的抖动。
func computeEffectiveMaxLifetime(base time.Duration, jitterPct int) time.Duration {
	if base <= 0 || jitterPct <= 0 {
		return base
	}
	if jitterPct > 50 {
		jitterPct = 50
	}

	cut := time.Duration(float64(base) * rand.Float64() * float64(jitterPct) / 100)
	return base - cut
}

// Stop 停止进程并释放命名空间资源。
func (w *Worker) Stop() error {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.State = router.WorkerClosing
		cmd := w.OvpnProcess
		namespace := w.Namespace
		done := w.processDone
		w.mu.Unlock()

		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				if err := cmd.Process.Kill(); err != nil {
					w.stopErr = fmt.Errorf("kill openvpn process: %w", err)
				}
				<-done
			}
		}

		if namespace != nil {
			if err := namespace.Destroy(); err != nil && w.stopErr == nil {
				w.stopErr = fmt.Errorf("destroy namespace: %w", err)
			}
		}
	})

	return w.stopErr
}
