package runner

import (
	"errors"
	"sync"
	"time"
)

// errRunTimeout is the cancel cause when a run exceeded its time limit.
var errRunTimeout = errors.New("run timeout")

// runClock is the time limit of a run that can be paused: the time the agent waits for the developer's
// permission decision does not count against the limit.
type runClock struct {
	mu        sync.Mutex
	timer     *time.Timer
	deadline  time.Time
	remaining time.Duration
	paused    int
}

func newRunClock(limit time.Duration, expire func()) *runClock {
	return &runClock{timer: time.AfterFunc(limit, expire), deadline: time.Now().Add(limit)}
}

// pause stops the clock (nested pauses are counted).
func (c *runClock) pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused++
	if c.paused == 1 && c.timer.Stop() {
		c.remaining = time.Until(c.deadline)
	} else if c.paused == 1 {
		c.remaining = 0 // already expired
	}
}

// resume starts the clock again with the time that was left.
func (c *runClock) resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused == 0 {
		return
	}
	c.paused--
	if c.paused == 0 && c.remaining > 0 {
		c.deadline = time.Now().Add(c.remaining)
		c.timer.Reset(c.remaining)
	}
}

func (c *runClock) stop() { c.timer.Stop() }
