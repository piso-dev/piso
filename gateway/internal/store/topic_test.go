package store

import (
	"sync"
	"testing"
)

func TestTopicFanoutSlowSubscriberAndCleanup(t *testing.T) {
	var topic topic[int]
	slow, cancelSlow := topic.subscribe()
	fast, cancelFast := topic.subscribe()
	for i := 0; i < 100; i++ {
		topic.publish(i)
		if got := <-fast; got != i {
			t.Fatalf("fast subscriber got %d want %d", got, i)
		}
	}
	if len(slow) != 64 {
		t.Fatal("slow subscriber wasn't bounded")
	}
	cancelSlow()
	cancelSlow()
	cancelFast()
	if len(topic.subs) != 0 {
		t.Fatal("subscriptions leaked")
	}
}

func TestTopicConcurrentPublishAndCancel(t *testing.T) {
	var topic topic[int]
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, cancel := topic.subscribe(); topic.publish(1); cancel(); cancel() }()
	}
	wg.Wait()
	if len(topic.subs) != 0 {
		t.Fatal("subscriptions leaked")
	}
}
