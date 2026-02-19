package singbox

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"marznode/internal/util"
)

type Runner struct {
	executablePath string
	currentConfig  string

	mu     sync.RWMutex
	cmd    *exec.Cmd
	logBus *util.LogBus
	exitCh chan struct{}
}

func NewRunner(executablePath string) *Runner {
	return &Runner{
		executablePath: executablePath,
		logBus:         util.NewLogBus(100),
		exitCh:         make(chan struct{}, 1),
	}
}

func (r *Runner) Start(ctx context.Context, configPath string) error {
	_ = ctx
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.runningLocked() {
		return errors.New("sing-box already running")
	}

	// Do not bind daemon lifetime to RPC/request context.
	cmd := exec.Command(r.executablePath, "run", "--disable-color", "-c", configPath)
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
	r.cmd = cmd
	r.currentConfig = configPath
	go r.capture(stderr)
	go r.capture(stdout)
	go r.wait(cmd)
	return nil
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
	r.mu.RLock()
	cmd := r.cmd
	r.mu.RUnlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	_ = cmd.Process.Signal(os.Interrupt)
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return ctx.Err()
	case <-timer.C:
		_ = cmd.Process.Kill()
		return nil
	case <-r.exitCh:
		return nil
	}
}

func (r *Runner) Restart(ctx context.Context, configPath string) error {
	if configPath == "" {
		r.mu.RLock()
		configPath = r.currentConfig
		r.mu.RUnlock()
	}
	if err := r.Stop(ctx); err != nil {
		return err
	}
	return r.Start(ctx, configPath)
}

func (r *Runner) Reload(ctx context.Context) error {
	r.mu.RLock()
	cmd := r.cmd
	r.mu.RUnlock()
	if cmd == nil || cmd.Process == nil {
		return errors.New("sing-box not running")
	}

	if runtime.GOOS == "windows" {
		return r.Restart(ctx, "")
	}

	return cmd.Process.Signal(syscall.SIGHUP)
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

	first := raw
	if idx := strings.IndexByte(raw, '\n'); idx >= 0 {
		first = strings.TrimSpace(raw[:idx])
	}
	if len(first) > 32 {
		return first[:32]
	}
	return first
}
