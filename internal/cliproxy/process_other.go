//go:build !windows

package cliproxy

import "os/exec"

func hideCoreWindow(cmd *exec.Cmd) {}
