//go:build linux

package worker

import (
	"context"
	"log"
	"time"

	"surfshark-proxy/internal/router"
)

const (
	preconnectInitialBackoff = 10 * time.Second
	preconnectMaxBackoff     = 60 * time.Second
	namespaceWaitBeforeRetry = 5 * time.Second
)

// StartPreconnectCoordinator 启动 preconnect 扫描协程。
func (m *Manager) StartPreconnectCoordinator(ctx context.Context) {
	if m.maxLifetime <= 0 {
		return
	}
	if m.preconnectDispatch == nil {
		m.preconnectDispatch = m.preconnectWorker
	}

	ctx = m.managedContext(ctx)
	go m.runPreconnectCoordinator(ctx)
}

func (m *Manager) runPreconnectCoordinator(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.preconnectSignal:
		}

		m.dispatchPending(ctx)
	}
}

func (m *Manager) dispatchPending(ctx context.Context) {
	m.mu.Lock()
	footprint := 0
	var candidates []*Worker
	graceForced := 0
	grace := m.effectiveGrace()
	for _, worker := range m.workers {
		switch worker.Info().State {
		case router.WorkerAwaitingReplacement:
			if grace > 0 {
				scheduled := worker.rotationScheduledSince()
				if !scheduled.IsZero() && time.Since(scheduled) >= grace {
					if worker.markClosingFromAwaiting() {
						graceForced++
						log.Printf("worker %s 宽限期到期仍无替换，强制下线", worker.ID)
					}
					if !worker.isClosingDrained() {
						footprint++
					}
					continue
				}
			}

			if _, busy := m.preconnectInflight[worker]; busy {
				footprint++
				continue
			}
			candidates = append(candidates, worker)
		case router.WorkerClosing:
			if !worker.isClosingDrained() {
				footprint++
			}
		}
	}

	for _, worker := range candidates {
		if m.minPoolSize > 0 && footprint >= m.minPoolSize {
			break
		}

		m.preconnectInflight[worker] = struct{}{}
		footprint++

		target := worker
		go m.preconnectDispatch(ctx, target)
	}
	m.mu.Unlock()

	if graceForced > 0 {
		m.signalWakeup()
		m.signalGrow()
	}
}

func (m *Manager) preconnectWorker(ctx context.Context, worker *Worker) {
	defer func() {
		m.mu.Lock()
		delete(m.preconnectInflight, worker)
		m.mu.Unlock()
	}()

	for {
		if ctx.Err() != nil {
			return
		}

		m.mu.RLock()
		used := len(m.usedIndexes)
		m.mu.RUnlock()
		if used < maxWorkerSlots-reservedForOnDemand {
			break
		}

		select {
		case <-time.After(namespaceWaitBeforeRetry):
		case <-ctx.Done():
			return
		}
	}

	select {
	case m.preconnectSem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() {
		<-m.preconnectSem
	}()

	backoff := preconnectInitialBackoff
	for {
		if ctx.Err() != nil {
			return
		}

		m.mu.RLock()
		stillOurs := m.workers[worker.ID] == worker && worker.isAwaitingReplacement()
		m.mu.RUnlock()
		if !stillOurs {
			return
		}

		if scheduled := worker.rotationScheduledSince(); !scheduled.IsZero() && time.Since(scheduled) >= m.effectiveGrace() {
			if worker.markClosingFromAwaiting() {
				log.Printf("worker %s 宽限期到期仍无替换，强制下线", worker.ID)
				m.signalWakeup()
				m.signalGrow()
			}
			return
		}

		createFn := m.createForCountryStaged
		if m.preconnectCreate != nil {
			createFn = m.preconnectCreate
		}

		newWorker, err := createFn(ctx, "")
		if err == nil {
			m.mu.Lock()
			stillOurs = m.rootCtx.Err() == nil && m.workers[worker.ID] == worker && worker.isAwaitingReplacement()
			if stillOurs {
				m.workers[newWorker.ID] = newWorker
			}
			m.mu.Unlock()

			if stillOurs {
				log.Printf("worker %s 替换就绪（新 worker = %s），开始 draining", worker.ID, newWorker.ID)
				if worker.markClosingFromAwaiting() {
					m.signalWakeup()
				}
				return
			}

			_ = newWorker.Stop()
			m.mu.Lock()
			m.releaseIndex(newWorker.Index)
			m.mu.Unlock()
			return
		}

		if ctx.Err() != nil {
			return
		}

		log.Printf("worker %s 替换失败（将在 %v 后重试）: %v", worker.ID, backoff, err)

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = nextBackoff(backoff)
	}
}

func nextBackoff(current time.Duration) time.Duration {
	next := current * 2
	if next > preconnectMaxBackoff {
		return preconnectMaxBackoff
	}
	return next
}

func (m *Manager) effectiveGrace() time.Duration {
	if m.rotationGrace > 0 {
		return m.rotationGrace
	}
	if m.maxLifetime <= 0 {
		return 0
	}

	auto := m.maxLifetime / 10
	maxGrace := 15 * time.Minute
	if auto > maxGrace {
		return maxGrace
	}
	return auto
}
