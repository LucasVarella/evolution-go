package chatwoot

import (
	"sync"
	"time"
)

// serialQueue runs tasks with the same key one at a time, in submission order,
// and tasks with different keys in parallel. Each chat gets its own key, so the
// messages of a conversation reach Chatwoot (and WhatsApp) in order without
// slowing other chats down.
type serialQueue struct {
	mu      sync.Mutex
	pending map[string][]func()
	wg      sync.WaitGroup
}

func newSerialQueue() *serialQueue {
	return &serialQueue{pending: make(map[string][]func())}
}

// Do schedules fn and returns immediately.
func (q *serialQueue) Do(key string, fn func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.wg.Add(1)
	if tasks, running := q.pending[key]; running {
		q.pending[key] = append(tasks, fn)
		return
	}
	q.pending[key] = []func(){}
	go q.run(key, fn)
}

func (q *serialQueue) run(key string, fn func()) {
	for fn != nil {
		func() {
			defer q.wg.Done()
			fn()
		}()

		q.mu.Lock()
		if tasks := q.pending[key]; len(tasks) > 0 {
			fn = tasks[0]
			q.pending[key] = tasks[1:]
		} else {
			delete(q.pending, key)
			fn = nil
		}
		q.mu.Unlock()
	}
}

// Wait blocks until every scheduled task has finished. Used by tests and on shutdown.
func (q *serialQueue) Wait() {
	q.wg.Wait()
}

// expiringSet remembers keys for a limited time.
type expiringSet struct {
	mu    sync.Mutex
	ttl   time.Duration
	items map[string]time.Time
	now   func() time.Time
}

func newExpiringSet(ttl time.Duration) *expiringSet {
	return &expiringSet{ttl: ttl, items: make(map[string]time.Time), now: time.Now}
}

func (s *expiringSet) Add(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, exp := range s.items {
		if now.After(exp) {
			delete(s.items, k)
		}
	}
	s.items[key] = now.Add(s.ttl)
}

// Has reports whether key was added and has not expired.
func (s *expiringSet) Has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.items[key]
	return ok && !s.now().After(exp)
}

func (s *expiringSet) Remove(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, key)
}
