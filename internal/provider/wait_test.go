package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestWaitFor(t *testing.T) {
	calls := 0
	err := waitFor(context.Background(), time.Second, func(context.Context) (bool, error) {
		calls++
		return calls == 3, nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("polling until done: err=%v calls=%d", err, calls)
	}

	boom := errors.New("boom")
	if err := waitFor(context.Background(), time.Second, func(context.Context) (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Errorf("a check error must stop polling: %v", err)
	}

	err = waitFor(context.Background(), 10*time.Millisecond, func(ctx context.Context) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	})
	if err == nil || !strings.Contains(err.Error(), "timed out after 10ms") {
		t.Errorf("a check cut off by the deadline must report a timeout: %v", err)
	}

	orig := pollInterval
	pollInterval = time.Hour
	t.Cleanup(func() { pollInterval = orig })
	err = waitFor(context.Background(), 10*time.Millisecond, func(context.Context) (bool, error) { return false, nil })
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("the deadline must interrupt the poll interval: %v", err)
	}
}

func TestWaitForKeepsPollingThroughTransientErrors(t *testing.T) {
	orig := pollInterval
	pollInterval = time.Millisecond
	t.Cleanup(func() { pollInterval = orig })

	unavailable := status.Error(codes.Unavailable, "upstream connect error")
	calls := 0
	err := waitFor(context.Background(), time.Second, func(context.Context) (bool, error) {
		calls++
		if calls < 3 {
			return false, fmt.Errorf("get database: %w", unavailable)
		}
		return true, nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("a rolling deploy must not fail the apply: err=%v calls=%d", err, calls)
	}

	err = waitFor(context.Background(), 20*time.Millisecond, func(context.Context) (bool, error) {
		return false, unavailable
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") || !errors.Is(err, unavailable) {
		t.Errorf("a timeout must carry the last transient error: %v", err)
	}

	denied := status.Error(codes.PermissionDenied, "no")
	if err := waitFor(context.Background(), time.Second, func(context.Context) (bool, error) { return false, denied }); !errors.Is(err, denied) {
		t.Errorf("a permanent status must stop polling: %v", err)
	}
}
