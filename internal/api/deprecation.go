package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

// minimumNotice is the shortest time between deprecating an operation and removing it while the API is v1alpha1.
// See docs/adr/0066-api-stability-and-deprecation.md.
const minimumNotice = 30 * 24 * time.Hour

type deprecation struct {
	since  time.Time
	sunset time.Time
}

// apply sets Deprecation (RFC 9745) and, when a removal date is set, Sunset (RFC 8594).
func (d deprecation) apply(h http.Header) {
	h.Set("Deprecation", "@"+strconv.FormatInt(d.since.Unix(), 10))
	if !d.sunset.IsZero() {
		h.Set("Sunset", d.sunset.Format(http.TimeFormat))
	}
}

// deprecations reads `deprecated: true` with x-samuori-deprecated (and optional x-samuori-sunset) dates from the
// contract. A deprecated operation without a valid date, or with less notice than the minimum, is an error.
func deprecations(spec *openapi3.T) (map[*openapi3.Operation]deprecation, error) {
	out := map[*openapi3.Operation]deprecation{}
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			d, ok, err := operationDeprecation(op)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", method, path, err)
			}
			if ok {
				out[op] = d
			}
		}
	}
	return out, nil
}

func operationDeprecation(op *openapi3.Operation) (deprecation, bool, error) {
	since, hasSince := op.Extensions["x-samuori-deprecated"]
	sunset, hasSunset := op.Extensions["x-samuori-sunset"]
	if !op.Deprecated {
		if hasSince || hasSunset {
			return deprecation{}, false, fmt.Errorf("deprecation dates without deprecated: true")
		}
		return deprecation{}, false, nil
	}
	var d deprecation
	var err error
	if d.since, err = contractDate(since); err != nil {
		return d, false, fmt.Errorf("x-samuori-deprecated: %w", err)
	}
	if hasSunset {
		if d.sunset, err = contractDate(sunset); err != nil {
			return d, false, fmt.Errorf("x-samuori-sunset: %w", err)
		}
		if d.sunset.Sub(d.since) < minimumNotice {
			return d, false, fmt.Errorf("x-samuori-sunset gives less than %d days' notice", int(minimumNotice.Hours()/24))
		}
	}
	return d, true, nil
}

func contractDate(v any) (time.Time, error) {
	s, _ := v.(string)
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("want a YYYY-MM-DD date, got %v", v)
	}
	return t, nil
}
