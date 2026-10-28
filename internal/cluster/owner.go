package cluster

import (
	"sync"

	"samuori.invalid/samuori/internal/ids"
)

// Ownership lets a service record which controller runs a piece of work and recognise work whose controller is gone.
type Ownership interface {
	ControllerID() string
	// Orphaned is an SQL condition, with its arguments, that holds when the controller named in column is not alive.
	Orphaned(column string) (string, []any)
}

func (m *Member) ControllerID() string { return m.ID }

func (m *Member) Orphaned(column string) (string, []any) {
	live, since := m.LiveSQL(column)
	return "(" + column + " IS NULL OR NOT " + live + ")", []any{since}
}

// Solo is the ownership of a process that runs without cluster membership, as in unit tests: work owned by any other
// process counts as orphaned.
type Solo struct {
	once sync.Once
	id   string
}

func (s *Solo) ControllerID() string {
	s.once.Do(func() { s.id = "solo-" + ids.New() })
	return s.id
}

func (s *Solo) Orphaned(column string) (string, []any) {
	return "(" + column + " IS NULL OR " + column + " <> ?)", []any{s.ControllerID()}
}
