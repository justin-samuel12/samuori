package rootlesshost

import (
	"errors"
	"fmt"
	"testing"

	"samuori.invalid/samuori/internal/engine/render"
	"samuori.invalid/samuori/internal/isolation"
)

func TestRenderSkewIsRequeuedAsInfrastructure(t *testing.T) {
	plan := []byte(`{"version":1,"method":"recipe"}`)
	skew := renderFailure(plan, fmt.Errorf("%w: unknown recipe %q", render.ErrNewerPlan, "rust-service"))
	if !errors.Is(skew, isolation.ErrInfrastructure) || !isolation.Requeue(skew) {
		t.Errorf("version skew must be an infrastructure failure: %v", skew)
	}
	content := renderFailure(plan, errors.New(`render: invalid build argument name "1bad"`))
	if errors.Is(content, isolation.ErrInfrastructure) || !errors.Is(content, isolation.ErrUnsupported) {
		t.Errorf("a render failure from the plan's content must stay an application failure: %v", content)
	}
}
