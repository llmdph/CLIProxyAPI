package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestFillFirstPoolIdleThenExpand(t *testing.T) {
	t.Parallel()
	p := newFillFirstPool()
	if !p.addAndOccupy("a") {
		t.Fatal("occupy a")
	}
	if got := p.occupyIdle([]string{"a", "b"}, nil); got != "" {
		t.Fatalf("idle while a busy = %q", got)
	}
	if !p.addAndOccupy("b") {
		t.Fatal("expand b")
	}
	p.release("a")
	if got := p.occupyIdle([]string{"a", "b"}, nil); got != "a" {
		t.Fatalf("idle after release = %q, want a", got)
	}
}

func TestFillFirstPoolMaxFive(t *testing.T) {
	t.Parallel()
	p := newFillFirstPool()
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if !p.addAndOccupy(id) {
			t.Fatalf("occupy %s", id)
		}
	}
	if p.addAndOccupy("f") {
		t.Fatal("should not expand past 5")
	}
}

func TestFillFirstPoolQueueAndBusy(t *testing.T) {
	t.Parallel()
	p := newFillFirstPool()
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if !p.addAndOccupy(id) {
			t.Fatalf("occupy %s", id)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errCh := make(chan error, 4)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- p.wait(ctx)
		}()
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		n := len(p.waiters)
		p.mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := p.wait(ctx); err == nil {
		t.Fatal("4th waiter should 429")
	} else if authErr, ok := err.(*Error); !ok || authErr.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("4th waiter err = %v", err)
	}
	p.release("a")
	p.release("b")
	p.release("c")
	wg.Wait()
	ok := 0
	for i := 0; i < 3; i++ {
		if err := <-errCh; err == nil {
			ok++
		}
	}
	if ok != 3 {
		t.Fatalf("woken waiters = %d, want 3", ok)
	}
}

func TestFillFirstPoolDropRemovesMember(t *testing.T) {
	t.Parallel()
	p := newFillFirstPool()
	if !p.addAndOccupy("a") {
		t.Fatal("occupy a")
	}
	p.drop("a")
	if got := p.occupyIdle([]string{"a"}, nil); got != "" {
		t.Fatalf("dropped member still idle-occupiable: %q", got)
	}
	if !p.addAndOccupy("a") {
		t.Fatal("re-add after drop")
	}
}

func TestFillFirstPoolAddIdle(t *testing.T) {
	t.Parallel()
	p := newFillFirstPool()
	if !p.addIdle("a") {
		t.Fatal("add idle a")
	}
	if !p.addIdle("a") {
		t.Fatal("add idle a again")
	}
	if got := p.occupyIdle([]string{"a"}, nil); got != "a" {
		t.Fatalf("occupy idle after addIdle = %q", got)
	}
	if p.addAndOccupy("a") {
		t.Fatal("occupy busy a")
	}
}

func TestFillFirstPoolDropIfIdle(t *testing.T) {
	t.Parallel()
	p := newFillFirstPool()
	if !p.addIdle("a") {
		t.Fatal("add idle a")
	}
	if !p.dropIfIdle("a") {
		t.Fatal("drop idle a")
	}
	if p.hasMember("a") {
		t.Fatal("idle member still present")
	}
	if !p.addAndOccupy("b") {
		t.Fatal("occupy b")
	}
	if p.dropIfIdle("b") {
		t.Fatal("dropIfIdle should refuse in-flight member")
	}
	if !p.hasMember("b") || !p.inFlightHas("b") {
		t.Fatal("in-flight member was dropped")
	}
	p.release("b")
	if !p.dropIfIdle("b") {
		t.Fatal("drop after release")
	}
	if p.hasMember("b") {
		t.Fatal("released member still present")
	}
}

