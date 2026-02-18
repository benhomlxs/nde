package util

import (
	"context"
	"errors"
	"time"
)

func Retry(ctx context.Context, attempts int, initialDelay time.Duration, fn func() error) error {
	if attempts <= 0 {
		attempts = 1
	}
	if initialDelay <= 0 {
		initialDelay = 100 * time.Millisecond
	}

	delay := initialDelay
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := fn(); err == nil {
			return nil
		} else {
			lastErr = err
		}

		if i == attempts-1 {
			break
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			if lastErr == nil {
				return ctx.Err()
			}
			return errors.Join(lastErr, ctx.Err())
		case <-timer.C:
		}
		delay *= 2
	}

	return lastErr
}
