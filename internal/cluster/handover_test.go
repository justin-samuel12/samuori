package cluster

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestHandOver(t *testing.T) {
	ctx := context.Background()
	d := removalDB(t)
	a, b := member(t, d, "a"), member(t, d, "b")
	a.beat(ctx)
	b.beat(ctx)
	ok, epoch := a.Leader()
	if !ok {
		t.Fatal("the first controller did not take the lease")
	}
	if err := b.HandOver(ctx, epoch, a.ID); !errors.Is(err, ErrNotLeader) {
		t.Errorf("a controller that does not lead handed over the lease: %v", err)
	}
	if err := a.HandOver(ctx, epoch, "nobody"); err == nil {
		t.Error("the lease went to a controller that is not alive")
	}
	if err := a.HandOver(ctx, epoch, b.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.Leader(); ok {
		t.Error("the old leader still thinks it leads")
	}
	err := d.InTx(ctx, func(tx *sql.Tx) error { return a.Fenced(ctx, tx, epoch) })
	if !errors.Is(err, ErrNotLeader) {
		t.Fatalf("the old leader could still write: %v", err)
	}
	a.beat(ctx)
	b.beat(ctx)
	if ok, _ := a.Leader(); ok {
		t.Error("the old leader took the lease back")
	}
	if ok, e := b.Leader(); !ok || e != epoch+1 {
		t.Fatalf("the new leader holds %v at epoch %d; want epoch %d", ok, e, epoch+1)
	}
}
