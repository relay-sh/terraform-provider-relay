package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var pollInterval = 10 * time.Second

func waitFor(ctx context.Context, timeout time.Duration, check func(context.Context) (bool, error)) error {
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var lastTransient error
	for {
		done, err := check(ctx)
		if !time.Now().Before(deadline) {
			if lastTransient != nil {
				return fmt.Errorf("timed out after %s, last error: %w", timeout, lastTransient)
			}
			return fmt.Errorf("timed out after %s", timeout)
		}
		switch {
		case err != nil && transient(err):
			lastTransient = err
		case err != nil:
			return err
		case done:
			return nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(pollInterval):
		}
	}
}

// transient reports whether polling should keep going: a rolling deploy in
// front of the API answers Unavailable, which is not the resource failing.
func transient(err error) bool {
	var s interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &s) {
		return false
	}
	switch s.GRPCStatus().Code() {
	case codes.Unavailable, codes.ResourceExhausted, codes.Aborted:
		return true
	}
	return false
}
