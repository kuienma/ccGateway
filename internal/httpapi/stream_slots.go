package httpapi

import (
	"context"
	"errors"
	"time"
)

var errStreamQueueTimeout = errors.New("等待流式并发槽位超时")

func (s *Server) acquireStream(ctx context.Context) error {
	timer := time.NewTimer(s.streamQueueTimeout)
	defer timer.Stop()
	select {
	case s.streamSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errStreamQueueTimeout
	}
}

func (s *Server) releaseStream() {
	<-s.streamSlots
}
