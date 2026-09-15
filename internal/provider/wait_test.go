package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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
