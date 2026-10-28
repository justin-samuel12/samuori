package api

import (
	"net/http"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/capacity"
	"samuori.invalid/samuori/internal/hostcap"
)

type volumeView struct {
	Label      string `json:"label"`
	Path       string `json:"path"`
	TotalBytes uint64 `json:"totalBytes"`
	FreeBytes  uint64 `json:"freeBytes"`
}

type hostCapacityView struct {
	CPUs                 int          `json:"cpus"`
	MemoryTotalBytes     uint64       `json:"memoryTotalBytes"`
	MemoryAvailableBytes uint64       `json:"memoryAvailableBytes"`
	Volumes              []volumeView `json:"volumes"`
	MeasuredAt           time.Time    `json:"measuredAt"`
}

func toHostCapacity(m capacity.Measured) *hostCapacityView {
	if m.Report == nil {
		return nil
	}
	r := hostcap.Clean(*m.Report)
	v := &hostCapacityView{CPUs: r.CPUs, MemoryTotalBytes: r.MemoryTotalBytes, MemoryAvailableBytes: r.MemoryAvailableBytes, Volumes: []volumeView{}, MeasuredAt: *m.At}
	for _, x := range r.Volumes {
		v.Volumes = append(v.Volumes, volumeView(x))
	}
	return v
}

type capacityControllerView struct {
	ID       string            `json:"id"`
	Hostname string            `json:"hostname"`
	Version  string            `json:"version"`
	Live     bool              `json:"live"`
	Leader   bool              `json:"leader"`
	Capacity *hostCapacityView `json:"capacity,omitempty"`
}

type capacityWorkerView struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Pool       string            `json:"pool"`
	State      string            `json:"state"`
	LastSeenAt *time.Time        `json:"lastSeenAt,omitempty"`
	Capacity   *hostCapacityView `json:"capacity,omitempty"`
}

type activeBuildView struct {
	ID            string     `json:"id"`
	ProjectID     string     `json:"projectId"`
	Project       string     `json:"project"`
	State         string     `json:"state"`
	Worker        string     `json:"worker"`
	AttemptsUsed  int        `json:"attemptsUsed"`
	MaxAttempts   int        `json:"maxAttempts"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	LeaseDeadline *time.Time `json:"leaseDeadline,omitempty"`
	QueueDeadline *time.Time `json:"queueDeadline,omitempty"`
	LeaseOverdue  bool       `json:"leaseOverdue"`
}

type openUploadView struct {
	ID            string    `json:"id"`
	ProjectID     string    `json:"projectId"`
	Project       string    `json:"project"`
	Status        string    `json:"status"`
	CreatedBy     string    `json:"createdBy"`
	ReceivedBytes int64     `json:"receivedBytes"`
	TotalBytes    int64     `json:"totalBytes"`
	CreatedAt     time.Time `json:"createdAt"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

// GetCapacity shows every host's last capacity report with the organization's unfinished builds and uploads
// (docs/operators/capacity.md).
func (a *API) GetCapacity(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !authz.Can(p, []authz.Role{authz.Administrator, authz.Operator}, authz.Resource{OrgID: p.OrgID}) {
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only administrators and operators see capacity")
		return
	}
	if a.capacity == nil {
		apigen.WriteProblem(w, http.StatusServiceUnavailable, "unavailable", "capacity reporting is not running on this controller")
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()
	out := struct {
		MeasuredAt  time.Time                `json:"measuredAt"`
		Controllers []capacityControllerView `json:"controllers"`
		Workers     []capacityWorkerView     `json:"workers"`
		Builds      []activeBuildView        `json:"builds"`
		Uploads     []openUploadView         `json:"uploads"`
	}{MeasuredAt: now, Controllers: []capacityControllerView{}, Workers: []capacityWorkerView{}, Builds: []activeBuildView{}, Uploads: []openUploadView{}}
	reports, err := a.capacity.Controllers(ctx)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if a.cluster != nil {
		list, err := a.cluster.Controllers(ctx)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		for _, c := range list {
			out.Controllers = append(out.Controllers, capacityControllerView{ID: c.ID, Hostname: c.Hostname, Version: c.Version, Live: c.Live, Leader: c.Leader, Capacity: toHostCapacity(reports[c.ID])})
		}
	}
	workers, err := a.capacity.Workers(ctx, p.OrgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	for _, x := range workers {
		out.Workers = append(out.Workers, capacityWorkerView{ID: x.ID, Name: x.Name, Pool: x.Pool, State: x.State, LastSeenAt: x.LastSeen, Capacity: toHostCapacity(x.Measured)})
	}
	builds, err := a.capacity.ActiveBuilds(ctx, p.OrgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	for _, b := range builds {
		out.Builds = append(out.Builds, activeBuildView{ID: b.ID, ProjectID: b.ProjectID, Project: b.Project, State: b.State, Worker: b.Worker, AttemptsUsed: b.AttemptsUsed, MaxAttempts: b.MaxAttempts,
			CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt, LeaseDeadline: b.LeaseDeadline, QueueDeadline: b.QueueDeadline, LeaseOverdue: b.LeaseDeadline != nil && b.LeaseDeadline.Before(now)})
	}
	uploads, err := a.capacity.OpenUploads(ctx, p.OrgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	for _, u := range uploads {
		out.Uploads = append(out.Uploads, openUploadView(u))
	}
	writeJSON(w, http.StatusOK, out)
}
