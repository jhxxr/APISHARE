package cliproxy

import (
	"context"
	"fmt"
	"time"
)

const poolRestartLimit = 3

var poolRestartDelay = time.Second
var poolStableWindow = time.Minute

func (m *Manager) stopRecovery() {
	m.mu.Lock()
	m.wantRun = false
	if m.recoveryCancel != nil {
		m.recoveryCancel()
	}
	m.mu.Unlock()
}

// Only the gateway owns this supervisor. The linked upstream SDK is unchanged.
func (m *Manager) recoverProcess() {
	m.op.Lock()
	m.mu.Lock()
	if !m.wantRun || m.cancel != nil || m.recoveryActive {
		m.mu.Unlock()
		m.op.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.recoveryActive = true
	m.recoveryCancel = cancel
	m.mu.Unlock()
	m.op.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		m.recoveryActive = false
		m.nextRestart = time.Time{}
		again := m.wantRun && m.cancel == nil && m.restartAttempts < poolRestartLimit
		m.mu.Unlock()
		if again {
			go m.recoverProcess()
		}
	}()
	for {
		m.mu.Lock()
		if !m.wantRun || m.restartAttempts >= poolRestartLimit || ctx.Err() != nil {
			if m.wantRun && m.restartAttempts >= poolRestartLimit {
				m.lastError = fmt.Sprintf("核心连续异常，%d 次自动恢复均未稳定运行，请检查配置后手动启动", poolRestartLimit)
			}
			m.mu.Unlock()
			return
		}
		delay := poolRestartDelay * time.Duration(1<<m.restartAttempts)
		m.nextRestart = time.Now().Add(delay)
		m.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		m.op.Lock()
		m.mu.Lock()
		if !m.wantRun || ctx.Err() != nil {
			m.mu.Unlock()
			m.op.Unlock()
			return
		}
		m.restartAttempts++
		m.nextRestart = time.Time{}
		m.mu.Unlock()
		err := m.start(ctx)
		m.mu.Lock()
		if err != nil {
			m.lastError = err.Error()
		}
		done := m.done
		m.mu.Unlock()
		m.op.Unlock()
		if err != nil {
			continue
		}
		m.Touch()
		go func() {
			timer := time.NewTimer(poolStableWindow)
			defer timer.Stop()
			select {
			case <-done:
				return
			case <-timer.C:
			}
			m.mu.Lock()
			if m.done == done && m.cancel != nil {
				m.restartAttempts = 0
			}
			m.mu.Unlock()
		}()
		return
	}
}
