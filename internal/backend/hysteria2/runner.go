package hysteria2

import (
	"bufio"
	"context"
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

	mu         sync.RWMutex
	cmd        *exec.Cmd
	logBus     *util.LogBus
	tempConfig string
}

func NewRunner(executablePath string) *Runner {
	return &Runner{
		executablePath: executablePath,
		logBus:         util.NewLogBus(100),
	}
}

func (r *Runner) Start(ctx context.Context, renderedConfig string) error {
	tmp, err := os.CreateTemp("", "hysteria-*.yaml")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(renderedConfig); err != nil {
		_ = tmp.Close()
		return err
	}
	_ = tmp.Close()

	cmd := exec.CommandContext(ctx, r.executablePath, "server", "-c", tmp.Name())
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

	r.mu.Lock()
	r.cmd = cmd
	r.tempConfig = tmp.Name()
	r.mu.Unlock()

	go r.capture(stderr)
	go r.capture(stdout)
	go r.wait()
	return nil
}

func (r *Runner) wait() {
	r.mu.RLock()
	cmd := r.cmd
	path := r.tempConfig
	r.mu.RUnlock()
	if cmd != nil {
		_ = cmd.Wait()
	}
	if path != "" {
		_ = os.Remove(path)
	}
	r.mu.Lock()
	r.cmd = nil
	r.tempConfig = ""
	r.mu.Unlock()
}

func (r *Runner) capture(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		r.logBus.Publish(scanner.Text())
	}
	r.logBus.Publish("")
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
	}
}

func (r *Runner) Running() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
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

func (r *Runner) Version(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, r.executablePath, "version").CombinedOutput()
	if err != nil {
		return ""
	}
	match := regexp.MustCompile(`Version:\s*v(\d+\.\d+\.\d+)`).FindStringSubmatch(string(out))
	if len(match) == 2 {
		return strings.TrimSpace(match[1])
	}
	return ""
}
