package solari

import (
	"errors"
	"testing"
)

// Gateway status + body code -> typed error (PROTOCOL.md section 4).
func TestMapGatewayErrorByCode(t *testing.T) {
	type want struct {
		auth, plan, concurrency, noCapacity bool // which typed error it must be; all false = a plain *GatewayError
	}
	cases := []struct {
		id     string
		status int
		code   string
		retry  bool
		w      want
	}{
		{"401", 401, "Unauthorized", false, want{auth: true}},
		{"402-plan", 402, "FeatureRequiresPlan", false, want{plan: true}},
		{"402-credit", 402, "InsufficientCredit", false, want{plan: true}},
		{"403-not-entitled", 403, "NotEntitled", false, want{plan: true}},
		{"403-other", 403, "Forbidden", false, want{}},
		{"404", 404, "NotFound", false, want{}},
		{"409-sandbox-stopped", 409, "SandboxStopped", false, want{}},
		{"410-gone", 410, "SandboxGone", false, want{}},
		{"429-concurrency-cap", 429, "ConcurrencyLimitExceeded", false, want{concurrency: true}},
		{"429-no-code", 429, "", false, want{concurrency: true}},
		{"429-too-many-executions", 429, "TooManyExecutions", true, want{}},
		{"429-object-limit", 429, "ObjectLimit", false, want{}},
		{"500-internal", 500, "InternalError", false, want{}},
		{"501-not-supported", 501, "NotSupported", false, want{}},
		{"503-no-capacity", 503, "NoCapacity", true, want{noCapacity: true}},
		{"503-start-timeout", 503, "StartTimeout", false, want{noCapacity: true}},
		{"503-no-code", 503, "", false, want{noCapacity: true}},
		{"503-sandbox-unavailable", 503, "SandboxUnavailable", false, want{}},
		{"503-store-unavailable", 503, "StoreUnavailable", true, want{}},
		{"503-admission-unavailable", 503, "AdmissionUnavailable", true, want{}},
		{"503-dependency-unavailable", 503, "DependencyUnavailable", true, want{}},
		{"507-insufficient-capacity", 507, "InsufficientCapacity", true, want{}},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			err := mapGatewayError(c.status, &GatewayErrorBody{Code: c.code, Retryable: c.retry})
			var auth *AuthError
			var plan *PlanError
			var conc *ConcurrencyLimitError
			var capErr *NoCapacityError
			if got := errors.As(err, &auth); got != c.w.auth {
				t.Errorf("AuthError = %v, want %v (%T)", got, c.w.auth, err)
			}
			if got := errors.As(err, &plan); got != c.w.plan {
				t.Errorf("PlanError = %v, want %v (%T)", got, c.w.plan, err)
			}
			if got := errors.As(err, &conc); got != c.w.concurrency {
				t.Errorf("ConcurrencyLimitError = %v, want %v (%T)", got, c.w.concurrency, err)
			}
			if got := errors.As(err, &capErr); got != c.w.noCapacity {
				t.Errorf("NoCapacityError = %v, want %v (%T)", got, c.w.noCapacity, err)
			}
			var g *GatewayError
			if !errors.As(err, &g) {
				t.Fatalf("every mapped error unwraps to *GatewayError, got %T", err)
			}
			if g.Status != c.status || g.Code != c.code {
				t.Errorf("status/code = %d/%q, want %d/%q", g.Status, g.Code, c.status, c.code)
			}
			if g.Body == nil || g.Body.Retryable != c.retry {
				t.Errorf("retryable hint not kept: %+v", g.Body)
			}
			if !c.w.auth && !c.w.plan && !c.w.concurrency && !c.w.noCapacity {
				if _, plainType := err.(*GatewayError); !plainType {
					t.Errorf("want a plain *GatewayError, got %T", err)
				}
			}
		})
	}
}
