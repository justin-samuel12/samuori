// Package topology works out how many failures each part of an installation tolerates (ADR 0046).
package topology

import "fmt"

type Tier string

const (
	Standalone Tier = "standalone"
	PartialHA  Tier = "partial-ha"
	HA         Tier = "ha"
)

type Database string

const (
	SQLite     Database = "sqlite"
	PostgreSQL Database = "postgresql"
)

type Storage string

const (
	Local Storage = "local"
	S3    Storage = "s3"
	OCI   Storage = "oci"
)

// Facts describe an installation, either as planned or as measured. Pools holds the worker count per enabled pool.
type Facts struct {
	Controllers        int
	Pools              map[string]int
	Database           Database
	DatabaseReplicated bool
	Storage            Storage
	StorageReplicated  bool
	PortalOnController bool
}

// Component is one part of the installation and the failures it survives.
type Component struct {
	Name      string `json:"name"`
	Have      string `json:"have"`
	Tolerates int    `json:"tolerates"`
	Declared  bool   `json:"declared,omitempty"`
	Gap       string `json:"gap,omitempty"`
}

type Assessment struct {
	Tier       Tier        `json:"tier"`
	Components []Component `json:"components"`
	Warnings   []string    `json:"warnings,omitempty"`
}

// Assess applies ADR 0046: HA when every component tolerates a failure, standalone when none does.
func Assess(f Facts) Assessment {
	var a Assessment
	add := func(c Component) { a.Components = append(a.Components, c) }

	ctl := Component{Name: "controllers", Have: count(f.Controllers, "controller"), Tolerates: max(0, (f.Controllers-1)/2)}
	switch {
	case f.Controllers < 3:
		ctl.Gap = fmt.Sprintf("add %d controller%s to reach 3", 3-f.Controllers, plural(3-f.Controllers))
	}
	if f.Controllers > 1 && f.Controllers%2 == 0 {
		a.Warnings = append(a.Warnings, fmt.Sprintf("%d controllers tolerate no more failures than %d; use an odd number", f.Controllers, f.Controllers-1))
	}
	add(ctl)

	wk := Component{Name: "workers", Tolerates: -1}
	if len(f.Pools) == 0 {
		wk.Have, wk.Tolerates, wk.Gap = "no workers", 0, "add 2 workers"
	}
	for pool, n := range f.Pools {
		t := max(0, n-1)
		if wk.Tolerates < 0 || t < wk.Tolerates {
			wk.Tolerates = t
		}
		if n < 2 {
			wk.Gap = fmt.Sprintf("add %d worker%s to pool %s", 2-n, plural(2-n), pool)
		}
	}
	if wk.Have == "" {
		total := 0
		for _, n := range f.Pools {
			total += n
		}
		wk.Have = fmt.Sprintf("%s in %s", count(total, "worker"), count(len(f.Pools), "pool"))
	}
	add(wk)

	db := Component{Name: "database"}
	switch {
	case f.Database == SQLite:
		db.Have, db.Gap = "SQLite on this host", "convert to a cluster with replicated PostgreSQL"
	case f.DatabaseReplicated:
		db.Have, db.Tolerates, db.Declared = "PostgreSQL, replicated", 1, true
	default:
		db.Have, db.Gap = "PostgreSQL, single instance", "use a replicated PostgreSQL, such as RDS Multi-AZ, and declare it"
	}
	add(db)

	st := Component{Name: "storage"}
	switch {
	case f.Storage == S3:
		st.Have, st.Tolerates = "S3", 1
	case f.Storage == OCI && f.StorageReplicated:
		st.Have, st.Tolerates, st.Declared = "OCI repository, replicated", 1, true
	case f.Storage == OCI:
		st.Have, st.Gap = "OCI repository", "declare the repository replicated, or use S3"
	default:
		st.Have, st.Gap = "local disk", "convert to a cluster with S3"
	}
	add(st)

	ep := Component{Name: "entry point"}
	if f.PortalOnController {
		ep.Have, ep.Gap = "the portal URL names a controller", "put a load balancer in front of the controllers"
	} else {
		ep.Have, ep.Tolerates = "a load balancer", 1
	}
	add(ep)

	all, none := true, true
	for _, c := range a.Components {
		if c.Tolerates > 0 {
			none = false
		} else {
			all = false
		}
	}
	switch {
	case all:
		a.Tier = HA
	case none:
		a.Tier = Standalone
	default:
		a.Tier = PartialHA
	}
	return a
}

func count(n int, noun string) string { return fmt.Sprintf("%d %s%s", n, noun, plural(n)) }

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
