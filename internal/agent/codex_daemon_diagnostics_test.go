package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestCodexDaemonStartupFailureExplainsRecoveryWithoutExposingOutput(t *testing.T) {
	for _, tc := range []struct {
		name, stderr, want string
		ctxErr, startErr   error
	}{
		{name: "launcher", startErr: errors.New("exit status 1"), stderr: "private-token this Windows launcher prevents background processes from outliving it", want: "后台启动器"},
		{name: "missing command", startErr: os.ErrNotExist, want: "未找到"},
		{name: "permission", startErr: os.ErrPermission, want: "权限"},
		{name: "timeout", ctxErr: context.DeadlineExceeded, startErr: errors.New("killed"), want: "超时"},
		{name: "failed command", startErr: errors.New("private-token"), stderr: "private-token", want: "启动命令执行失败"},
		{name: "not ready", want: "尚不可连接"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := codexDaemonStartupFailure(tc.ctxErr, tc.startErr, tc.stderr)
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "原会话保留") {
				t.Fatalf("missing cause or recovery: %v", err)
			}
			if strings.Contains(err.Error(), "private-token") {
				t.Fatal("raw CLI output escaped into user-visible error")
			}
		})
	}
}

func TestCodexDaemonDiagnosticCaptureIsBounded(t *testing.T) {
	var output codexDaemonDiagnostics
	input := strings.Repeat("x", 20000)
	for range 3 {
		if n, err := output.Write([]byte(input)); err != nil || n != len(input) {
			t.Fatal("capture interrupted child output", n, err)
		}
	}
	if len(output.String()) > 16384 || len(output.String()) == 0 {
		t.Fatal("diagnostic memory is unbounded or empty")
	}
}
