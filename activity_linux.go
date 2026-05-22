//go:build linux

package main

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"
)

func systemBootTimeMS() int64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "btime ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				sec, err := strconv.ParseInt(parts[1], 10, 64)
				if err == nil && sec > 0 {
					return sec * 1000
				}
			}
			break
		}
	}
	return 0
}

type IdleDetector struct {
	threshold time.Duration
	callback  func(bool)
	cancel    context.CancelFunc
	wasIdle   bool
	lastInput time.Time
}

func NewIdleDetector(threshold time.Duration, callback func(idle bool)) *IdleDetector {
	return &IdleDetector{
		threshold: threshold,
		callback:  callback,
		lastInput: time.Now(),
	}
}

func (id *IdleDetector) Start(ctx context.Context) {
	ctx, id.cancel = context.WithCancel(ctx)
	go id.loop(ctx)
}

func (id *IdleDetector) Stop() {
	if id.cancel != nil {
		id.cancel()
	}
}

func (id *IdleDetector) loop(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			// Simple idle detection: if no activity received, assume idle
			idleDuration := time.Since(id.lastInput)
			isIdle := idleDuration >= id.threshold
			if isIdle != id.wasIdle {
				id.wasIdle = isIdle
				if id.callback != nil {
					id.callback(isIdle)
				}
			}
		}
	}
}
