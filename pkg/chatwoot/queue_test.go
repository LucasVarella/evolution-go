package chatwoot

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSerialQueueKeepsOrderPerKey(t *testing.T) {
	q := newSerialQueue()
	var mu sync.Mutex
	got := map[string][]int{}

	for i := 0; i < 50; i++ {
		for _, key := range []string{"a", "b"} {
			i, key := i, key
			q.Do(key, func() {
				if i%7 == 0 {
					time.Sleep(time.Millisecond) // make out-of-order execution likely if the queue were wrong
				}
				mu.Lock()
				got[key] = append(got[key], i)
				mu.Unlock()
			})
		}
	}
	q.Wait()

	for _, key := range []string{"a", "b"} {
		if len(got[key]) != 50 {
			t.Fatalf("key %s ran %d tasks, want 50", key, len(got[key]))
		}
		for i, v := range got[key] {
			if v != i {
				t.Fatalf("key %s out of order: %v", key, got[key])
			}
		}
	}
}

func TestSerialQueueRunsKeysInParallel(t *testing.T) {
	q := newSerialQueue()
	release := make(chan struct{})
	var otherRan atomic.Bool

	q.Do("slow", func() { <-release })
	q.Do("fast", func() { otherRan.Store(true) })

	deadline := time.Now().Add(2 * time.Second)
	for !otherRan.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	q.Wait()

	if !otherRan.Load() {
		t.Fatal("a blocked key prevented another key from running")
	}
}

func TestSerialQueueNeverRunsSameKeyConcurrently(t *testing.T) {
	q := newSerialQueue()
	var running, maxRunning atomic.Int32

	for i := 0; i < 30; i++ {
		q.Do("chat", func() {
			n := running.Add(1)
			for {
				m := maxRunning.Load()
				if n <= m || maxRunning.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(200 * time.Microsecond)
			running.Add(-1)
		})
	}
	q.Wait()

	if maxRunning.Load() != 1 {
		t.Fatalf("same key ran %d tasks at once", maxRunning.Load())
	}
}

func TestExpiringSet(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newExpiringSet(time.Minute)
	s.now = func() time.Time { return now }

	s.Add("x")
	if !s.Has("x") {
		t.Fatal("x should be present")
	}
	if s.Has("y") {
		t.Fatal("y was never added")
	}

	now = now.Add(2 * time.Minute)
	if s.Has("x") {
		t.Fatal("x should have expired")
	}

	s.Add("z")
	s.Remove("z")
	if s.Has("z") {
		t.Fatal("z was removed")
	}
}
