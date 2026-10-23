package cliproxy

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestPoolRecoversCrashesAndRespectsManualStop(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	manager, err := NewManager(t.TempDir(), port)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	wait := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(11 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("supervisor state: %+v", manager.Status())
	}
	crash := func() {
		t.Helper()
		manager.mu.Lock()
		process, done := manager.process, manager.done
		manager.mu.Unlock()
		if process == nil {
			t.Fatal("pool has no live process")
		}
		if err := process.Kill(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	for attempt := 1; attempt <= poolRestartLimit; attempt++ {
		crash()
		wait(func() bool { s := manager.Status(); return s.Running && !s.Restarting && s.RestartAttempts == attempt })
	}
	crash()
	wait(func() bool { return manager.Status().RecoveryExhausted })
	if manager.Running() {
		t.Fatal("unbounded crash restart")
	}
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	crash()
	if err := manager.Stop(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1400 * time.Millisecond)
	status := manager.Status()
	if status.Running || status.Restarting || status.AutoStart {
		t.Fatalf("manual stop was restarted: %+v", status)
	}
	again, err := NewManager(manager.root, port)
	if err != nil || again.Status().AutoStart {
		t.Fatal("manual stop did not survive restart")
	}
}
