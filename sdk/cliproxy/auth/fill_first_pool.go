package auth

import (
	"context"
	"net/http"
	"sync"
	"time"
)

const (
	fillFirstPoolMax     = 5
	fillFirstQueueMax    = 3
	fillFirstWaitTimeout = 60 * time.Second
)

type fillFirstHoldKey struct{}

type fillFirstHold struct {
	id       string
	pool     *fillFirstPool
	noRetry  bool
	acquired bool
}

type fillFirstPool struct {
	mu            sync.Mutex
	members       []string
	inFlight      map[string]struct{}
	inFlightTimes map[string]time.Time
	waiters       []chan struct{}
	gen           uint64
}

func newFillFirstPool() *fillFirstPool {
	return &fillFirstPool{
		inFlight:      make(map[string]struct{}),
		inFlightTimes: make(map[string]time.Time),
	}
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
		if p.inFlightTimes == nil {
			p.inFlightTimes = make(map[string]time.Time)
		}
		p.inFlight[id] = struct{}{}
		p.inFlightTimes[id] = time.Now()
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
	if p.inFlightTimes == nil {
		p.inFlightTimes = make(map[string]time.Time)
	}
	p.inFlight[id] = struct{}{}
	p.inFlightTimes[id] = time.Now()
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
	p.bumpLocked()
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
	if p.inFlightTimes != nil {
		delete(p.inFlightTimes, id)
	}
	p.bumpLocked()
}

func (p *fillFirstPool) drop(id string) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.inFlight, id)
	if p.inFlightTimes != nil {
		delete(p.inFlightTimes, id)
	}
	kept := p.members[:0]
	for _, member := range p.members {
		if member != id {
			kept = append(kept, member)
		}
	}
	p.members = kept
	p.bumpLocked()
}

func (p *fillFirstPool) pruneMissing(alive map[string]struct{}) []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	removed := make([]string, 0)
	kept := p.members[:0]
	for _, member := range p.members {
		if _, ok := alive[member]; !ok {
			delete(p.inFlight, member)
			if p.inFlightTimes != nil {
				delete(p.inFlightTimes, member)
			}
			removed = append(removed, member)
			continue
		}
		kept = append(kept, member)
	}
	p.members = kept
	if len(removed) > 0 {
		p.bumpLocked()
	}
	return removed
}

func (p *fillFirstPool) pruneStaleInFlight(maxAge time.Duration) int {
	if p == nil || maxAge <= 0 {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.inFlightTimes) == 0 {
		return 0
	}
	now := time.Now()
	var purged int
	for id, t := range p.inFlightTimes {
		if now.Sub(t) > maxAge {
			delete(p.inFlight, id)
			delete(p.inFlightTimes, id)
			purged++
		}
	}
	if purged > 0 {
		p.bumpLocked()
	}
	return purged
}

func (p *fillFirstPool) snapshot() (memberCount, inFlight int, waiterCount int, gen uint64) {
	if p == nil {
		return 0, 0, 0, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.members), len(p.inFlight), len(p.waiters), p.gen
}

func (p *fillFirstPool) wait(ctx context.Context, gen uint64) error {
	if p == nil {
		return fillFirstBusyError()
	}
	p.mu.Lock()
	if p.gen != gen {
		p.mu.Unlock()
		return nil
	}
	if len(p.waiters) >= fillFirstQueueMax {
		p.mu.Unlock()
		return fillFirstBusyError()
	}
	ch := make(chan struct{}, 1)
	p.waiters = append(p.waiters, ch)
	p.mu.Unlock()
	timer := time.NewTimer(fillFirstWaitTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		p.mu.Lock()
		p.removeWaiterLocked(ch)
		p.mu.Unlock()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fillFirstBusyError()
	case <-timer.C:
		p.mu.Lock()
		p.removeWaiterLocked(ch)
		p.mu.Unlock()
		return fillFirstBusyError()
	case <-ch:
		return nil
	}
}

func (p *fillFirstPool) bumpLocked() {
	p.gen++
	p.wakeLocked()
}

func (p *fillFirstPool) wakeLocked() {
	if len(p.waiters) == 0 {
		return
	}
	waiters := p.waiters
	p.waiters = nil
	for _, ch := range waiters {
		select {
		case ch <- struct{}{}:
		default:
		}
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
