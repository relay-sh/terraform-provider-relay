package provider

import (
	"context"
	"fmt"
	"time"
)

var pollInterval = 10 * time.Second

func waitFor(ctx context.Context, timeout time.Duration, check func(context.Context) (bool, error)) error {
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for {
		done, err := check(ctx)
		if !time.Now().Before(deadline) {
			return fmt.Errorf("timed out after %s", timeout)
		}
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(pollInterval):
		}
	}
}
