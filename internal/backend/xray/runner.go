package xray

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"marznode/internal/util"
)

type Runner struct {
	executablePath string
	assetsPath     string

	mu     sync.RWMutex
	cmd    *exec.Cmd
	logBus *util.LogBus
	exitCh chan struct{}
}

func NewRunner(executablePath, assetsPath string) *Runner {
	return &Runner{
		executablePath: executablePath,
		assetsPath:     assetsPath,
		logBus:         util.NewLogBus(100),
		exitCh:         make(chan struct{}, 1),
	}
}

func (r *Runner) Start(ctx context.Context, configJSON string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.runningLocked() {
		return errors.New("xray already running")
	}

	cmd := exec.CommandContext(ctx, r.executablePath, "run", "-config", "stdin:")
	cmd.Env = append(cmd.Env, "XRAY_LOCATION_ASSET="+r.assetsPath)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}
	select {
	case <-r.exitCh:
	default:
	}

	if _, err := io.WriteString(stdin, configJSON); err != nil {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		return err
	}
	_ = stdin.Close()

	r.cmd = cmd
	go r.capture(stderr)
	go r.capture(stdout)
	go r.wait(cmd)

	return r.waitStartup(ctx, 6*time.Second)
}

func (r *Runner) waitStartup(ctx context.Context, timeout time.Duration) error {
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sub := r.logBus.Subscribe()
	defer r.logBus.Unsubscribe(sub)

	for {
		select {
		case <-tctx.Done():
			return nil
		case line, ok := <-sub:
			if !ok {
				return nil
			}
			if strings.Contains(line, "started") {
				return nil
			}
		case <-r.exitCh:
			return errors.New("xray exited during startup")
		}
	}
}

func (r *Runner) capture(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		r.logBus.Publish(scanner.Text())
	}
	r.logBus.Publish("")
}

func (r *Runner) wait(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	_ = cmd.Wait()

	sendExit := false
	r.mu.Lock()
	if r.cmd == cmd {
		r.cmd = nil
		sendExit = true
	}
	r.mu.Unlock()

	if sendExit {
		select {
		case r.exitCh <- struct{}{}:
		default:
		}
	}
}

func (r *Runner) Stop(ctx context.Context) error {
	r.mu.Lock()
	cmd := r.cmd
	r.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}

	_ = cmd.Process.Signal(os.Interrupt)
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()

	select {
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return ctx.Err()
	case <-deadline.C:
		_ = cmd.Process.Kill()
		return nil
	case <-r.exitCh:
		return nil
	}
}

func (r *Runner) Running() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.runningLocked()
}

func (r *Runner) runningLocked() bool {
	return r.cmd != nil && r.cmd.Process != nil && r.cmd.ProcessState == nil
}

func (r *Runner) Logs(includeBuffer bool) <-chan string {
	out := make(chan string, 256)
	sub := r.logBus.Subscribe()

	go func() {
		defer close(out)
		defer r.logBus.Unsubscribe(sub)

		if includeBuffer {
			for _, line := range r.logBus.Snapshot() {
				out <- line
			}
		}
		for line := range sub {
			out <- line
		}
	}()

	return out
}

func (r *Runner) ExitChannel() <-chan struct{} {
	return r.exitCh
}

func (r *Runner) Version(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, r.executablePath, "version").CombinedOutput()
	if err != nil {
		return ""
	}

	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return ""
	}

	// Keep version format DB-safe for panels that store backend version in VARCHAR(32).
	if m := regexp.MustCompile(`\b\d+\.\d+\.\d+\b`).FindString(raw); m != "" {
		return m
	}

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "Xray ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "Xray "))
		}
		if len(line) > 32 {
			return line[:32]
		}
		return line
	}
	return ""
}

func (r *Runner) Restart(ctx context.Context, configJSON string) error {
	if err := r.Stop(ctx); err != nil {
		return fmt.Errorf("xray stop failed: %w", err)
	}
	return r.Start(ctx, configJSON)
}
