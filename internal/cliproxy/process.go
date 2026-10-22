package cliproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	sdk "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	_ "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
)

const childConfigEnv = "APISHARE_INTERNAL_CLIPROXY_CONFIG"

func init() {
	path := os.Getenv(childConfigEnv)
	if path == "" {
		return
	}
	// This also works in test binaries, so isolation is exercised by the same
	// lifecycle tests as production without installing a second executable.
	os.Unsetenv(childConfigEnv)
	err := runPoolCore(path)
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "CLIProxyAPI pool stopped")
		os.Exit(1)
	}
	os.Exit(0)
}

func runPoolCore(path string) error {
	cfg, err := config.LoadConfig(path)
	var state diskState
	if err == nil {
		var raw []byte
		raw, err = os.ReadFile(filepath.Join(filepath.Dir(path), "state.json"))
		if err == nil {
			err = json.Unmarshal(raw, &state)
		}
	}
	if err == nil {
		cfg.Host = "127.0.0.1"
		cfg.Port = state.Port
		cfg.AuthDir = filepath.Join(filepath.Dir(path), "auths")
		cfg.RemoteManagement.AllowRemote = false
		cfg.RemoteManagement.DisableControlPanel = true
		builder := sdk.NewBuilder().WithConfig(cfg).WithConfigPath(path)
		recorder, statsErr := openAccountUptimeRecorder(filepath.Dir(path))
		if statsErr == nil {
			defer recorder.db.Close()
			builder.WithResultPolicy(recorder)
			usage.RegisterNamedPlugin("apishare-account-attempts", recorder)
			builder.WithServerOptions(recorder.traceOption())
		} else {
			fmt.Fprintln(os.Stderr, "CLIProxyAPI account uptime is temporarily unavailable")
		}
		builder.WithServerOptions(poolRuntimeOption(state, recorder))
		var service *sdk.Service
		service, err = builder.Build()
		if err == nil {
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				_, _ = io.Copy(io.Discard, os.Stdin)
				cancel() // Closing the parent's pipe also handles a crashed parent.
			}()
			err = service.Run(ctx)
			cancel()
		}
	}
	return err
}

func (m *Manager) startProcess(path string) (chan struct{}, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(executable)
	cmd.Env = append(os.Environ(), childConfigEnv+"="+path)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	hideCoreWindow(cmd)
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		input.Close()
		return nil, fmt.Errorf("无法启动内置账号池：%w", err)
	}
	done := make(chan struct{})
	cancel := func() {
		input.Close()
		go func() {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
			}
		}()
	}
	m.mu.Lock()
	m.cancel, m.done, m.lastError = cancel, done, ""
	m.mu.Unlock()
	go func() {
		err := cmd.Wait()
		input.Close()
		m.mu.Lock()
		m.cancel = nil
		if err != nil {
			m.lastError = "账号池已停止，请检查运行状态后重试"
		}
		close(done)
		m.mu.Unlock()
	}()
	return done, nil
}
