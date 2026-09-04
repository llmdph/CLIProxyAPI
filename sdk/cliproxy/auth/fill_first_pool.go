package auth

import (
	"context"
	"net/http"
	"sync"
)

const (
	fillFirstPoolMax  = 5
	fillFirstQueueMax = 3
)

type fillFirstHoldKey struct{}

type fillFirstHold struct {
	id       string
	pool     *fillFirstPool
	noRetry  bool
	acquired bool
}

type fillFirstPool struct {
	mu       sync.Mutex
	members  []string
	inFlight map[string]struct{}
	waiters  []chan struct{}
}

func newFillFirstPool() *fillFirstPool {
	return &fillFirstPool{inFlight: make(map[string]struct{})}
}

func fillFirstBusyError() error {
	return &Error{
		Code:       "resource_exhausted",
		Message:    "too many concurrent requests",
		Retryable:  true,
		HTTPStatus: http.StatusTooManyRequests,
	}
}

func (p *fillFirstPool) occupyIdle(candidates []string, tried map[string]struct{}) string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range p.members {
		if id == "" {
			continue
		}
		if _, used := tried[id]; used {
			continue
		}
		if _, busy := p.inFlight[id]; busy {
			continue
		}
		ok := false
		for _, candidate := range candidates {
			if candidate == id {
				ok = true
				break
			}
		}
		if !ok {
			continue
		}
		p.inFlight[id] = struct{}{}
		return id
	}
	return ""
}

func (p *fillFirstPool) addAndOccupy(id string) bool {
	if p == nil || id == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, busy := p.inFlight[id]; busy {
		return false
	}
	if !p.addLocked(id) {
		return false
	}
	p.inFlight[id] = struct{}{}
	return true
}

func (p *fillFirstPool) addIdle(id string) bool {
	if p == nil || id == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.addLocked(id)
}

func (p *fillFirstPool) addLocked(id string) bool {
	for _, member := range p.members {
		if member == id {
			return true
		}
	}
	if len(p.members) >= fillFirstPoolMax {
		return false
	}
	p.members = append(p.members, id)
	return true
}

func (p *fillFirstPool) hasMember(id string) bool {
	if p == nil || id == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, member := range p.members {
		if member == id {
			return true
		}
	}
	return false
}

func (p *fillFirstPool) inFlightHas(id string) bool {
	if p == nil || id == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.inFlight[id]
	return ok
}

func (p *fillFirstPool) dropIfIdle(id string) bool {
	if p == nil || id == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, busy := p.inFlight[id]; busy {
		return false
	}
	kept := p.members[:0]
	found := false
	for _, member := range p.members {
		if member == id {
			found = true
			continue
		}
		kept = append(kept, member)
	}
	if !found {
		return false
	}
	p.members = kept
	p.wakeLocked()
	return true
}

func (p *fillFirstPool) memberIDs() []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.members))
	copy(out, p.members)
	return out
}

func (p *fillFirstPool) release(id string) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.inFlight, id)
	p.wakeLocked()
}

func (p *fillFirstPool) drop(id string) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.inFlight, id)
	kept := p.members[:0]
	for _, member := range p.members {
		if member != id {
			kept = append(kept, member)
		}
	}
	p.members = kept
	p.wakeLocked()
}

func (p *fillFirstPool) pruneMissing(alive map[string]struct{}) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := p.members[:0]
	for _, member := range p.members {
		if _, ok := alive[member]; !ok {
			delete(p.inFlight, member)
			continue
		}
		kept = append(kept, member)
	}
	p.members = kept
}

func (p *fillFirstPool) wait(ctx context.Context) error {
	if p == nil {
		return fillFirstBusyError()
	}
	p.mu.Lock()
	if len(p.waiters) >= fillFirstQueueMax {
		p.mu.Unlock()
		return fillFirstBusyError()
	}
	ch := make(chan struct{}, 1)
	p.waiters = append(p.waiters, ch)
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		p.mu.Lock()
		p.removeWaiterLocked(ch)
		p.mu.Unlock()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fillFirstBusyError()
	case <-ch:
		return nil
	}
}

func (p *fillFirstPool) wakeLocked() {
	if len(p.waiters) == 0 {
		return
	}
	ch := p.waiters[0]
	p.waiters = p.waiters[1:]
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (p *fillFirstPool) removeWaiterLocked(ch chan struct{}) {
	kept := p.waiters[:0]
	for _, waiter := range p.waiters {
		if waiter != ch {
			kept = append(kept, waiter)
		}
	}
	p.waiters = kept
}
