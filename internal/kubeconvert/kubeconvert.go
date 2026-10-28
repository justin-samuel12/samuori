// Package kubeconvert generates Kubernetes deployment artifacts from a project's Compose file, source or image with an
// approved provider run on a worker, then validates them on the controller (ADR 0048,
// docs/architecture/kubernetes-conversion.md). Samuori generates and validates; it never applies anything to a cluster.
package kubeconvert

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/convertjob"
)

var (
	ErrForbidden = errors.New("forbidden")
	ErrNotFound  = errors.New("not found")
	ErrConflict  = errors.New("conflict")
)

type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

func invalid(format string, args ...any) error { return &Invalid{fmt.Sprintf(format, args...)} }

// Input kinds.
const (
	InputCompose     = "compose"
	InputSource      = "source"
	InputBuild       = "build"
	InputPublication = "publication"
)

// Input says what is converted. A Compose input may name a Samuori build or publication whose image replaces Service's.
type Input struct {
	Kind          string `json:"kind"`
	SnapshotID    string `json:"snapshotId,omitempty"`
	ComposePath   string `json:"composePath,omitempty"`
	SourceDir     string `json:"sourceDir,omitempty"`
	BuildRunID    string `json:"buildRunId,omitempty"`
	PublicationID string `json:"publicationId,omitempty"`
	Service       string `json:"service,omitempty"`
}

// Output formats.
const (
	FormatManifests = "manifests"
	FormatHelm      = "helm"
	FormatKustomize = "kustomize"
)

// Exposure types for the entry service.
const (
	ExposeClusterIP    = "cluster-ip"
	ExposeNodePort     = "node-port"
	ExposeLoadBalancer = "load-balancer"
	ExposeIngress      = "ingress"
)

type Exposure struct {
	Service string `json:"service,omitempty"`
	Type    string `json:"type,omitempty"`
	Host    string `json:"host,omitempty"`
	Path    string `json:"path,omitempty"`
}

type Resources struct {
	CPURequest    string `json:"cpuRequest,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	CPULimit      string `json:"cpuLimit,omitempty"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
}

// Storage chooses how Compose volumes become Kubernetes volumes; hostPath is never offered (Samuori policy).
type Storage struct {
	Volumes      string `json:"volumes,omitempty"`
	Size         string `json:"size,omitempty"`
	StorageClass string `json:"storageClass,omitempty"`
}

type Options struct {
	Format            string    `json:"format"`
	Namespace         string    `json:"namespace,omitempty"`
	Replicas          int       `json:"replicas,omitempty"`
	Exposure          Exposure  `json:"exposure"`
	Resources         Resources `json:"resources"`
	Storage           Storage   `json:"storage"`
	KubernetesVersion string    `json:"kubernetesVersion,omitempty"`
}

// Conversion states. A conversion is ready once a revision exists; ready never means applied anywhere.
const (
	StateQueued       = "queued"
	StateAssigned     = "assigned"
	StateRunning      = "running"
	StateNeedsAnswers = "needs-answers"
	StateSucceeded    = "succeeded"
	StateFailed       = "failed"
	StateTimedOut     = "timed-out"
	StateCancelled    = "cancelled"
)

func activeState(s string) bool { return s == StateQueued || s == StateAssigned || s == StateRunning }

type Conversion struct {
	ID, OrgID, ProjectID, Name, Provider string
	Input                                Input
	Options                              Options
	State                                string
	CurrentRevision                      int
	CreatedBy                            string
	CreatedAt, UpdatedAt                 time.Time
	LatestRun                            *Run
}

type Run struct {
	ID, ConversionID     string
	Number               int
	Options              Options
	Answers              map[string]string
	ProviderVersion      string
	State                string
	FailureClass, Detail string
	Questions            []Question
	Revision             int
	ToolDigest           string
	OperationID          string
	CreatedBy            string
	CreatedAt, UpdatedAt time.Time
	EndedAt              *time.Time
}

type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Digest  string `json:"digest"`
	Size    int    `json:"size"`
}

// Finding severities. An error is a policy violation or an invalid object; review needs operator judgement.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
	SeverityReview  = "review"
	SeverityInfo    = "info"
)

type Finding struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	File     string `json:"file,omitempty"`
	Object   string `json:"object,omitempty"`
	Field    string `json:"field,omitempty"`
	Source   string `json:"source,omitempty"`
}

// Lineage records everything a revision came from; it never holds credentials.
type Lineage struct {
	ProjectID         string  `json:"projectId"`
	Input             Input   `json:"input"`
	SnapshotDigest    string  `json:"snapshotDigest,omitempty"`
	SpecID            string  `json:"specId,omitempty"`
	SpecRevision      int     `json:"specRevision,omitempty"`
	BuildRunID        string  `json:"buildRunId,omitempty"`
	ImageDigest       string  `json:"imageDigest,omitempty"`
	ImageReference    string  `json:"imageReference,omitempty"`
	Provider          string  `json:"provider"`
	ProviderVersion   string  `json:"providerVersion,omitempty"`
	ToolDigest        string  `json:"toolDigest,omitempty"`
	Options           Options `json:"options"`
	KubernetesVersion string  `json:"kubernetesVersion"`
	RunID             string  `json:"runId,omitempty"`
	WorkerID          string  `json:"workerId,omitempty"`
	IsolationProfile  string  `json:"isolationProfile,omitempty"`
	EditedFrom        int     `json:"editedFrom,omitempty"`
}

type Revision struct {
	Number    int
	RunID     string
	Parent    int
	Origin    string
	Findings  []Finding
	Lineage   Lineage
	Errors    int
	CreatedBy string
	CreatedAt time.Time
	Files     []File
}

// Revision origins.
const (
	OriginProvider = "provider"
	OriginEdit     = "edit"
)

// Question is something a provider would have asked interactively: Default is the provider's own default and
// Answer the value the run used, the user's answer or the default (Move2Kube's review → answer → resume, ADR 0048).
// Kind is Select, MultiSelect, Confirm, Input or MultiLineInput; list values are JSON arrays.
type Question struct {
	ID      string   `json:"id"`
	Text    string   `json:"text"`
	Kind    string   `json:"kind"`
	Hints   []string `json:"hints,omitempty"`
	Options []string `json:"options,omitempty"`
	Default string   `json:"default,omitempty"`
	Answer  string   `json:"answer,omitempty"`
}

// KubernetesVersion is the version Samuori validates against; see schemas/.
const KubernetesVersion = "1.36"

var (
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	hostRe   = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	quantity = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(m|Ki|Mi|Gi|Ti|k|M|G|T)?$`)
	ingPath  = regexp.MustCompile(`^/[A-Za-z0-9._~/-]{0,200}$`)
)

func (o *Options) normalize(p Provider) error {
	if o.Format == "" {
		o.Format = FormatManifests
	}
	if !slices.Contains(p.Formats(), o.Format) {
		return invalid("%s cannot produce %s; choose %s", p.Name(), o.Format, strings.Join(p.Formats(), " or "))
	}
	if o.Namespace != "" && !dnsLabel.MatchString(o.Namespace) {
		return invalid("the namespace %q must be a DNS label (lowercase letters, digits and dashes)", o.Namespace)
	}
	if o.Replicas < 0 || o.Replicas > 100 {
		return invalid("replicas must be between 1 and 100")
	}
	if o.Replicas == 0 {
		o.Replicas = 1
	}
	e := &o.Exposure
	if e.Type == "" {
		e.Type = ExposeClusterIP
	}
	switch e.Type {
	case ExposeClusterIP, ExposeNodePort, ExposeLoadBalancer:
		if e.Host != "" || e.Path != "" {
			return invalid("an ingress host and path need the ingress exposure")
		}
	case ExposeIngress:
		if !hostRe.MatchString(e.Host) {
			return invalid("the ingress host %q must be a DNS name such as shop.example.com", e.Host)
		}
		if e.Path == "" {
			e.Path = "/"
		}
		if !ingPath.MatchString(e.Path) {
			return invalid("the ingress path must start with / and use URL path characters only")
		}
	default:
		return invalid("unknown exposure %q", e.Type)
	}
	if e.Service != "" && !dnsLabel.MatchString(e.Service) {
		return invalid("the exposed service %q must be a DNS label", e.Service)
	}
	if e.Type != ExposeClusterIP && e.Service == "" {
		return invalid("choose the service to expose")
	}
	for _, q := range []string{o.Resources.CPURequest, o.Resources.CPULimit, o.Resources.MemoryRequest, o.Resources.MemoryLimit, o.Storage.Size} {
		if q != "" && !quantity.MatchString(q) {
			return invalid("%q is not a Kubernetes quantity such as 250m, 512Mi or 1Gi", q)
		}
	}
	switch o.Storage.Volumes {
	case "":
		o.Storage.Volumes = "persistentVolumeClaim"
	case "persistentVolumeClaim", "emptyDir":
	case "hostPath":
		return invalid("hostPath volumes are not offered: they expose the node's filesystem (Samuori policy)")
	default:
		return invalid("volumes are persistentVolumeClaim or emptyDir")
	}
	if o.Storage.StorageClass != "" && !dnsLabel.MatchString(o.Storage.StorageClass) {
		return invalid("the storage class must be a DNS label")
	}
	if o.KubernetesVersion == "" {
		o.KubernetesVersion = KubernetesVersion
	}
	if o.KubernetesVersion != KubernetesVersion {
		return invalid("Samuori validates against Kubernetes %s only", KubernetesVersion)
	}
	return nil
}

func (in *Input) normalize() error {
	switch in.Kind {
	case InputCompose:
		if in.SnapshotID == "" || !convertjob.LocalPath(in.ComposePath) {
			return invalid("a Compose input needs an upload and the Compose file's path inside it")
		}
		if in.BuildRunID != "" && in.PublicationID != "" {
			return invalid("choose a build or a publication for the image, not both")
		}
		if (in.BuildRunID != "" || in.PublicationID != "") && !dnsLabel.MatchString(in.Service) {
			return invalid("name the Compose service whose image the Samuori image replaces")
		}
	case InputSource:
		if in.SnapshotID == "" || (in.SourceDir != "" && !convertjob.LocalPath(in.SourceDir)) {
			return invalid("a source input needs an upload and, optionally, a directory inside it")
		}
		if in.BuildRunID != "" && in.PublicationID != "" {
			return invalid("choose a build or a publication for the image, not both")
		}
		if (in.BuildRunID != "" || in.PublicationID != "") && !dnsLabel.MatchString(in.Service) {
			return invalid("name the service whose image the Samuori image replaces")
		}
	case InputBuild, InputPublication:
		if (in.Kind == InputBuild) == (in.BuildRunID == "") || (in.Kind == InputPublication) == (in.PublicationID == "") {
			return invalid("an image input names exactly one build or publication")
		}
		if in.Service == "" {
			in.Service = "app"
		}
		if !dnsLabel.MatchString(in.Service) {
			return invalid("the service name must be a DNS label")
		}
	default:
		return invalid("the input is a compose file, source, build or publication")
	}
	return nil
}
