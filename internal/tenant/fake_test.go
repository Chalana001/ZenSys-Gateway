package tenant

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// fakeRepository is an in-memory Repository used to exercise the lifecycle
// rules without a database.
//
// It mirrors the real repository's contract, including the compare-and-swap
// behaviour of Transition and the fact that a no-op returns ErrConflict rather
// than silently succeeding.
type fakeRepository struct {
	mu     sync.Mutex
	routes map[string]Route
	events []Event

	// Failure injection, so the service's error propagation can be tested
	// without a real database outage.
	getErr        error
	reserveErr    error
	transitionErr error
	releaseErr    error
	pingErr       error

	// Call counters, used to prove that no-op paths do not write.
	reserveCalls    int
	transitionCalls int
	releaseCalls    int
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{routes: make(map[string]Route)}
}

var fakeNow = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

func (f *fakeRepository) GetBySlug(_ context.Context, slug string) (Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.getErr != nil {
		return Route{}, f.getErr
	}

	route, ok := f.routes[slug]
	if !ok {
		return Route{}, fmt.Errorf("%w: %q", ErrNotFound, slug)
	}
	return route, nil
}

func (f *fakeRepository) List(_ context.Context, filter ListFilter) ([]Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	slugs := make([]string, 0, len(f.routes))
	for slug := range f.routes {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	out := make([]Route, 0, len(slugs))
	for _, slug := range slugs {
		if filter.After != "" && slug <= filter.After {
			continue
		}

		route := f.routes[slug]
		if filter.Product != nil && route.Product != *filter.Product {
			continue
		}
		if filter.Status != nil && route.Status != *filter.Status {
			continue
		}

		out = append(out, route)
		if len(out) >= filter.ClampLimit() {
			break
		}
	}

	return out, nil
}

func (f *fakeRepository) Reserve(_ context.Context, route Route, event Event) (Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.reserveCalls++
	if f.reserveErr != nil {
		return Route{}, f.reserveErr
	}
	if _, exists := f.routes[route.TenantSlug]; exists {
		return Route{}, fmt.Errorf("%w: %q", ErrAlreadyExists, route.TenantSlug)
	}

	route.CreatedAt = fakeNow
	route.UpdatedAt = fakeNow
	f.routes[route.TenantSlug] = route
	f.events = append(f.events, event)

	return route, nil
}

func (f *fakeRepository) Transition(_ context.Context, slug string, from, to Status, event Event) (Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.transitionCalls++
	if f.transitionErr != nil {
		return Route{}, f.transitionErr
	}

	route, ok := f.routes[slug]
	if !ok {
		return Route{}, fmt.Errorf("%w: %q", ErrNotFound, slug)
	}
	if route.Status != from {
		return Route{}, fmt.Errorf("%w: %q moved from %s to %s", ErrConflict, slug, from, route.Status)
	}

	route.Status = to
	route.UpdatedAt = route.UpdatedAt.Add(time.Second)
	f.routes[slug] = route
	f.events = append(f.events, event)

	return route, nil
}

func (f *fakeRepository) Release(_ context.Context, slug string, event Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.releaseCalls++
	if f.releaseErr != nil {
		return f.releaseErr
	}
	if _, ok := f.routes[slug]; !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, slug)
	}

	delete(f.routes, slug)
	f.events = append(f.events, event)
	return nil
}

func (f *fakeRepository) Ping(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pingErr
}

// --- test helpers ---

func (f *fakeRepository) seed(route Route) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[route.TenantSlug] = route
}

func (f *fakeRepository) statusOf(slug string) Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.routes[slug].Status
}

func (f *fakeRepository) exists(slug string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.routes[slug]
	return ok
}

func (f *fakeRepository) recordedEvents() []Event {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]Event, len(f.events))
	copy(out, f.events)
	return out
}
