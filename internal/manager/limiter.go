package manager

import "sync"

// Limiter bounds the number of concurrent operations and can be resized at
// runtime. Each Acquire returns the release function of the slot it took, so
// resizing never makes a holder release into a different semaphore.
//
// It replaces sizedwaitgroup for image thumbnail generation: setting
// SizedWaitGroup.Size after creation has no effect (its channel keeps the
// initial capacity), which left on-the-fly thumbnails strictly serialised.
type Limiter struct {
	mu  sync.Mutex
	sem chan struct{}
}

// NewLimiter returns a limiter allowing n concurrent holders (minimum 1).
func NewLimiter(n int) *Limiter {
	l := &Limiter{}
	l.Resize(n)
	return l
}

// Resize changes the capacity for future acquisitions. Current holders keep
// their slots in the previous semaphore.
func (l *Limiter) Resize(n int) {
	if n < 1 {
		n = 1
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sem != nil && cap(l.sem) == n {
		return
	}
	l.sem = make(chan struct{}, n)
}

// Acquire blocks until a slot is free and returns its release function.
func (l *Limiter) Acquire() (release func()) {
	l.mu.Lock()
	sem := l.sem
	l.mu.Unlock()

	sem <- struct{}{}
	var once sync.Once
	return func() { once.Do(func() { <-sem }) }
}

// Size returns the current capacity.
func (l *Limiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return cap(l.sem)
}
