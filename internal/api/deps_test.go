package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"samuori.invalid/samuori/internal/capacity"
	"samuori.invalid/samuori/internal/cluster"
	"samuori.invalid/samuori/internal/dbcredentials"
	"samuori.invalid/samuori/internal/selfupdate"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/adapters/git/gogit"
	oidcadapter "samuori.invalid/samuori/internal/adapters/identity/oidc"
	"samuori.invalid/samuori/internal/admission"
	"samuori.invalid/samuori/internal/approvals"
	"samuori.invalid/samuori/internal/artifacts"
	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/auth"
	"samuori.invalid/samuori/internal/builds"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/discovery/jobs"
	"samuori.invalid/samuori/internal/discovery/sandbox"
	"samuori.invalid/samuori/internal/federation"
	"samuori.invalid/samuori/internal/health"
	"samuori.invalid/samuori/internal/identity"
	"samuori.invalid/samuori/internal/imagesources"
	"samuori.invalid/samuori/internal/kubeconvert"
	"samuori.invalid/samuori/internal/notify"
	"samuori.invalid/samuori/internal/observability"
	"samuori.invalid/samuori/internal/operations"
	"samuori.invalid/samuori/internal/pipelines"
	"samuori.invalid/samuori/internal/pki"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/providers"
	"samuori.invalid/samuori/internal/publication"
	"samuori.invalid/samuori/internal/recipes"
	"samuori.invalid/samuori/internal/secrets"
	"samuori.invalid/samuori/internal/signing"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/specs"
	"samuori.invalid/samuori/internal/storage"
	"samuori.invalid/samuori/internal/tenancy"
	"samuori.invalid/samuori/internal/topology"
	"samuori.invalid/samuori/internal/validation"
	"samuori.invalid/samuori/internal/workers"
)

// testRedirect is never fetched: tests read the provider's redirect and call the callback themselves.
const testRedirect = "http://127.0.0.1/api/v1/auth/oidc/callback"

// testTags resolves every tag to a digest derived from its reference, except tags named "missing"; tests never
// reach a public registry.
type testTags struct{}

func (testTags) ResolveTag(_ context.Context, ref string) (string, error) {
	if strings.HasSuffix(ref, ":missing") {
		return "", errors.New("the tag missing does not exist")
	}
	sum := sha256.Sum256([]byte(ref))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// testDeps wires every real service against d, as cmd/samuori-server does.
func testDeps(t *testing.T, d *db.DB, store *tenancy.Store, log *slog.Logger, h *health.Registry, dir string, secure bool, cfg sources.Config) Deps {
	t.Helper()
	blobs, err := storage.NewBlobs(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	ops := operations.NewStore(d)
	src, err := sources.NewService(d, blobs, ops, store, log, filepath.Join(dir, "uploads"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(src.Wait)
	runner := sandbox.Runner{Command: []string{os.Args[0], "child-discover"}, Limits: sandbox.DefaultLimits(), Namespaces: true}
	cat, err := recipes.LoadBuiltin()
	if err != nil {
		t.Fatal(err)
	}
	policies := policy.NewStore(d, cat)
	disc := jobs.NewService(d, src, ops, runner, policies, log)
	t.Cleanup(disc.Wait)
	recipeStatus := recipes.NewStatuses(d, cat)
	recipeStatus.UseImagePolicy(policies.BaseImageRefusal)
	specSvc := specs.NewService(d, store, policies)
	ca, err := pki.Open(filepath.Join(dir, "pki"))
	if err != nil {
		t.Fatal(err)
	}
	workerStore := workers.NewStore(d, ca)
	checker := admission.NewChecker(specSvc, src, recipeStatus, policies, disc, workerStore)
	checker.UseTagResolver(testTags{})
	art, err := artifacts.NewStore(d, blobs, filepath.Join(dir, "pki"))
	if err != nil {
		t.Fatal(err)
	}
	if h == nil {
		h = health.NewRegistry(time.Second)
	}
	sec, err := secrets.NewService(d, filepath.Join(dir, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	checker.UseBindings(sec)
	providerStore := providers.NewStore(d)
	checker.UseMethodPolicy(providerStore)
	buildSvc := builds.NewService(d, store, specSvc, src, recipeStatus, policies, checker)
	buildSvc.UseSecrets(sec)
	buildSvc.UsePlacement(workerStore)
	val := validation.NewService(d, blobs, store, policies, nil, nil, log)
	pubs := publication.NewService(d, blobs, store, sec, val, publication.OrasPusher{}, log)
	appr := approvals.NewService(d, store)
	pubs.UseApprovals(appr)
	appr.UseBaseImagePolicy(policies)
	appr.UseEgressPolicy(policies)
	checker.UseEgressAllowances(appr)
	buildSvc.UseEgressAllowances(appr)
	signingKeys := signing.NewStore(d, sec)
	pubs.UseSigning(signingKeys)
	pubs.UseDownloadPolicy(policies.DeveloperDownloads)
	idStore := identity.NewStore(d, sec)
	member, err := cluster.Join(context.Background(), d, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	dbCreds, err := dbcredentials.New(d, member, dir, "sqlite://"+filepath.Join(dir, "samuori.db"), make([]byte, 32), log)
	if err != nil {
		t.Fatal(err)
	}
	return Deps{Database: dbCreds, Cluster: member, SelfUpdate: selfupdate.New(d, nil, member, nil, "arm64", log), Capacity: capacity.New(d, member.ID, dir, log), Log: log, Health: h, Tenancy: store, Auth: auth.NewService(d, store, auth.DefaultPolicy()), DataDir: dir, SecureCookies: secure, Sources: src, Operations: ops, Discovery: disc,
		Specs: specSvc, Admission: checker, Recipes: recipeStatus, Policies: policies,
		Workers: workerStore, DB: d, Builds: buildSvc, Secrets: sec, Validations: val, Publications: pubs, Approvals: appr, Signing: signingKeys, Artifacts: art,
		Retention: &audit.Retention{DB: d, Dir: filepath.Join(dir, "audit-export")}, Federation: federation.NewStore(d), Diagnostics: observability.Source{DB: d, Health: h},
		Identity: identity.NewService(idStore, oidcadapter.New(), nil, testRedirect), IdentityStore: idStore, Topology: topology.NewStore(d), Providers: providerStore, Kube: kubeconvert.NewService(d, store, src, blobs, providerStore, ops), TopologyInfo: TopologyInfo{Storage: topology.Local, ExternalURL: "http://127.0.0.1:8080"}, ImageSources: imagesources.NewStore(d, sec), Notify: notify.NewService(d, sec),
		Pipelines: pipelines.NewService(d, sec, src, buildSvc, specSvc, store, gogit.Fetcher{Dir: t.TempDir()})}
}
