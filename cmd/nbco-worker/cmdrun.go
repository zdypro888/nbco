package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
)

const (
	commandOutputLimit       = 256 << 10
	commandProgressEvery     = 5 * time.Second
	commandProgressMinEvery  = 2 * time.Second
	commandProgressMinOutput = 8 << 10
)

type commandResult struct {
	Output   string
	ExitCode int
}

func runCommandExec(ctx context.Context, dir, command string, progress func(string)) (commandResult, error) {
	bin, args := shellCommand(command)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	// WaitDelay：sh 退出后若孙进程（nohup/后台守护）仍持有 stdout/stderr 管道，
	// 10 秒后强制关闭——否则 Run 的内部拷贝 goroutine 永不结束，worker 主循环
	// 被一条「启动了后台进程」的命令永久钉死，超时与服务端取消都救不回。
	cmd.WaitDelay = 10 * time.Second
	out := newCommandOutput(progress)
	cmd.Stdout = out
	cmd.Stderr = out

	waitErr := cmd.Run()
	res := commandResult{Output: out.String(), ExitCode: exitCode(waitErr)}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if waitErr != nil && res.ExitCode == -1 {
		return res, waitErr
	}
	return res, nil
}

func runCommandPTY(ctx context.Context, dir, command string, progress func(string)) (commandResult, error) {
	bin, args := shellCommand(command)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: termCols, Rows: termRows})
	if err != nil {
		return commandResult{ExitCode: -1}, err
	}
	defer func() { _ = ptmx.Close() }()

	var out limitedBuffer
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 32<<10)
		lastProgress := time.Now()
		sinceProgress := 0
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				chunk := string(buf[:n])
				out.Write(buf[:n])
				sinceProgress += n
				since := time.Since(lastProgress)
				if progress != nil && (since >= commandProgressEvery || (sinceProgress >= commandProgressMinOutput && since >= commandProgressMinEvery)) {
					progress(tailText(chunk, 60))
					lastProgress = time.Now()
					sinceProgress = 0
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-ctx.Done():
		_ = ptmx.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-waitDone
		waitErr = ctx.Err()
	}
	<-readDone

	res := commandResult{Output: out.String(), ExitCode: exitCode(waitErr)}
	if errors.Is(waitErr, context.Canceled) || errors.Is(waitErr, context.DeadlineExceeded) {
		return res, waitErr
	}
	if waitErr != nil && res.ExitCode == -1 {
		return res, waitErr
	}
	return res, nil
}

func shellCommand(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/C", command}
	}
	return "/bin/sh", []string{"-lc", command}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

type limitedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) {
	if len(p) >= commandOutputLimit {
		b.buf.Reset()
		_, _ = b.buf.Write(p[len(p)-commandOutputLimit:])
		b.truncated = true
		return
	}
	if overflow := b.buf.Len() + len(p) - commandOutputLimit; overflow > 0 {
		_ = b.buf.Next(overflow)
		b.truncated = true
	}
	_, _ = b.buf.Write(p)
}

func (b *limitedBuffer) String() string {
	s := b.buf.String()
	if b.truncated {
		s = "[前序输出已截断]\n" + s
	}
	return strings.TrimSpace(s)
}

type commandOutput struct {
	mu            sync.Mutex
	buf           limitedBuffer
	progress      func(string)
	lastProgress  time.Time
	sinceProgress int
}

func newCommandOutput(progress func(string)) *commandOutput {
	return &commandOutput{progress: progress, lastProgress: time.Now()}
}

func (o *commandOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.buf.Write(p)
	o.sinceProgress += len(p)
	since := time.Since(o.lastProgress)
	if o.progress != nil && (since >= commandProgressEvery || (o.sinceProgress >= commandProgressMinOutput && since >= commandProgressMinEvery)) {
		o.progress(tailText(string(p), 60))
		o.lastProgress = time.Now()
		o.sinceProgress = 0
	}
	return len(p), nil
}

func (o *commandOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func tailText(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func writeCommandScript(dir, command string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "command.txt"), []byte(command), 0o644)
}

func commandSummary(command, mode string, res commandResult, err error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "命令执行完成，退出码：%d\n", res.ExitCode)
	fmt.Fprintf(&b, "执行模式：%s\n", mode)
	if err != nil {
		fmt.Fprintf(&b, "错误：%v\n", err)
	}
	b.WriteString("命令：\n")
	b.WriteString(command)
	if strings.TrimSpace(res.Output) != "" {
		b.WriteString("\n\n输出：\n")
		_, _ = io.WriteString(&b, res.Output)
	}
	return b.String()
}
