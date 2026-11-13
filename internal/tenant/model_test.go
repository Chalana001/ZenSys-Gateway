package tenant

import (
	"errors"
	"testing"
)

// TestCanTransition is the exhaustive state machine table.
//
// Every (from, to) pair is listed, including the illegal ones, so that adding
// a new status without deciding its transitions fails here rather than
// silently defaulting to "not allowed".
func TestCanTransition(t *testing.T) {
	all := []Status{StatusProvisioning, StatusActive, StatusDisabled}

	legal := map[[2]Status]bool{
		// Reserve creates a route directly in PROVISIONING, so nothing
		// transitions *into* it.
		{StatusProvisioning, StatusActive}:   true,
		{StatusProvisioning, StatusDisabled}: true,

		// The documented PROVISIONING -> ACTIVE -> DISABLED path.
		{StatusActive, StatusDisabled}: true,

		// Re-enable without re-provisioning.
		{StatusDisabled, StatusActive}: true,
	}

	for _, from := range all {
		for _, to := range all {
			want := legal[[2]Status{from, to}]
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				if got := CanTransition(from, to); got != want {
					t.Fatalf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
				}
			})
		}
	}
}

// TestCanTransition_RejectsSelfTransitions pins the decision that idempotency
// is a service-layer concern, not a transition-table one.
func TestCanTransition_RejectsSelfTransitions(t *testing.T) {
	for _, s := range []Status{StatusProvisioning, StatusActive, StatusDisabled} {
		if CanTransition(s, s) {
			t.Errorf("CanTransition(%s, %s) should be false; idempotency is handled in the service", s, s)
		}
	}
}

// TestCanTransition_RejectsUnknownStatuses checks that an unrecognised value
// on either side fails closed.
func TestCanTransition_RejectsUnknownStatuses(t *testing.T) {
	if CanTransition("PENDING", StatusActive) {
		t.Error("an unknown source status must not permit a transition")
	}
	if CanTransition(StatusProvisioning, "PENDING") {
		t.Error("an unknown target status must not be permitted")
	}
	if CanTransition("", "") {
		t.Error("empty statuses must not permit a transition")
	}
}

func TestAllowedTransitions_ReturnsACopy(t *testing.T) {
	first := AllowedTransitions(StatusProvisioning)
	if len(first) == 0 {
		t.Fatal("PROVISIONING should have at least one allowed transition")
	}

	first[0] = "TAMPERED"

	if second := AllowedTransitions(StatusProvisioning); second[0] == "TAMPERED" {
		t.Fatal("AllowedTransitions returned a reference to the package-level table")
	}
}

func TestProductValid(t *testing.T) {
	for _, p := range Products() {
		if !p.Valid() {
			t.Errorf("Products() returned %q which Valid() rejects", p)
		}
	}

	for _, bad := range []Product{"", "pos", "Pos", "ERP", "POS ", " GMS"} {
		if bad.Valid() {
			t.Errorf("Product(%q).Valid() = true, want false", bad)
		}
	}
}

func TestStatusValidAndRoutable(t *testing.T) {
	tests := []struct {
		status   Status
		valid    bool
		routable bool
	}{
		{StatusProvisioning, true, false},
		{StatusActive, true, true},
		{StatusDisabled, true, false},
		{"PENDING", false, false},
		{"", false, false},
		{"active", false, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			if got := tt.status.Valid(); got != tt.valid {
				t.Errorf("Valid() = %v, want %v", got, tt.valid)
			}
			if got := tt.status.Routable(); got != tt.routable {
				t.Errorf("Routable() = %v, want %v", got, tt.routable)
			}
		})
	}
}

// TestRouteRoutable checks that only ACTIVE routes serve traffic. Both
// PROVISIONING and DISABLED must fail closed, and neither may reach a product.
func TestRouteRoutable(t *testing.T) {
	tests := []struct {
		status Status
		want   bool
	}{
		{StatusProvisioning, false},
		{StatusActive, true},
		{StatusDisabled, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			route := Route{TenantSlug: "mangala", Product: ProductPOS, Status: tt.status}
			if got := route.Routable(); got != tt.want {
				t.Fatalf("Route{%s}.Routable() = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestParseProduct(t *testing.T) {
	p, err := ParseProduct("POS")
	if err != nil {
		t.Fatalf("ParseProduct(POS): %v", err)
	}
	if p != ProductPOS {
		t.Fatalf("ParseProduct(POS) = %q", p)
	}

	_, err = ParseProduct("pos")
	if err == nil {
		t.Fatal("expected a lowercase product name to be rejected")
	}
	if !errors.Is(err, ErrInvalidProduct) {
		t.Fatalf("expected ErrInvalidProduct, got: %v", err)
	}
}

func TestParseStatus(t *testing.T) {
	s, err := ParseStatus("ACTIVE")
	if err != nil {
		t.Fatalf("ParseStatus(ACTIVE): %v", err)
	}
	if s != StatusActive {
		t.Fatalf("ParseStatus(ACTIVE) = %q", s)
	}

	_, err = ParseStatus("active")
	if err == nil {
		t.Fatal("expected a lowercase status to be rejected")
	}
	if !errors.Is(err, ErrInvalidStatus) {
		t.Fatalf("expected ErrInvalidStatus, got: %v", err)
	}
}

func TestActionValid(t *testing.T) {
	for _, a := range []Action{ActionReserve, ActionActivate, ActionDisable, ActionRelease} {
		if !a.Valid() {
			t.Errorf("Action(%q).Valid() = false", a)
		}
	}
	for _, bad := range []Action{"", "reserve", "DELETE", "CREATE"} {
		if bad.Valid() {
			t.Errorf("Action(%q).Valid() = true, want false", bad)
		}
	}
}

func TestListFilterClampLimit(t *testing.T) {
	tests := []struct {
		limit int
		want  int
	}{
		{0, DefaultListLimit},
		{-5, DefaultListLimit},
		{10, 10},
		{MaxListLimit, MaxListLimit},
		{MaxListLimit + 1, MaxListLimit},
		{1000000, MaxListLimit},
	}

	for _, tt := range tests {
		f := ListFilter{Limit: tt.limit}
		if got := f.ClampLimit(); got != tt.want {
			t.Errorf("ListFilter{Limit: %d}.ClampLimit() = %d, want %d", tt.limit, got, tt.want)
		}
	}
}
