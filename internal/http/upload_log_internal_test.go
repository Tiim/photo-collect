package http

import (
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"
)

func TestInterruptCause(t *testing.T) {
	for _, tc := range []struct {
		err  error
		d    time.Duration
		want string
	}{
		{io.ErrUnexpectedEOF, 45689 * time.Millisecond, causeClientGone},
		{io.ErrUnexpectedEOF, 60001 * time.Millisecond, causeProxyTimeout},
		{fmt.Errorf("read body: %w", io.ErrUnexpectedEOF), 59600 * time.Millisecond, causeProxyTimeout},
		{io.ErrUnexpectedEOF, 120300 * time.Millisecond, causeProxyTimeout},
		{io.ErrUnexpectedEOF, 500 * time.Millisecond, causeClientGone},
		{io.ErrUnexpectedEOF, 90 * time.Second, causeClientGone},
		{fmt.Errorf("read: %w", os.ErrDeadlineExceeded), 30 * time.Second, causeIdle},
		{errChunkAborted, 60 * time.Second, causeSuperseded},
		{errors.New("disk full"), time.Second, causeOther},
	} {
		if got := interruptCause(tc.err, tc.d); got != tc.want {
			t.Errorf("interruptCause(%v, %v) = %s, want %s", tc.err, tc.d, got, tc.want)
		}
	}
}
