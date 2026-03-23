package harness

import "sync"

// Counter is a simple thread-safe incrementing counter.
type Counter struct {
	mu    sync.Mutex
	value int
}

// Increment atomically increments the counter and returns the new value.
func (c *Counter) Increment() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value++
	return c.value
}
