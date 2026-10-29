package auth

import (
	"strings"
	"testing"
)

// Other packages' tests lower Argon2Params for speed; this pins the production cost so a lowered value cannot ship.
func TestPasswordHashUsesProductionCost(t *testing.T) {
	if p := Argon2Params; p.Time != 3 || p.Memory != 64*1024 || p.Threads != 2 {
		t.Fatalf("argon2id defaults = %+v, want t=3 m=65536 p=2", p)
	}
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h, "$m=65536,t=3,p=2$") {
		t.Fatalf("hash %q does not record the production cost", h)
	}
	if !VerifyPassword(h, "correct horse battery") || VerifyPassword(h, "wrong horse battery") {
		t.Fatal("verification accepted a wrong password or refused the right one")
	}
}
