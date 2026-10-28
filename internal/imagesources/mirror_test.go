package imagesources

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	ggname "github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"samuori.invalid/samuori/internal/testsupport/registrytest"
)

func TestAuthorizingMirror(t *testing.T) {
	f := setup(t)
	up := registrytest.Upstream(t, "mirror", "upstream-pull-password")
	upstream := up.Addr
	push := func(repo string) (string, string) {
		img, _ := random.Image(256, 2)
		ref, _ := ggname.ParseReference(upstream+"/"+repo+":1", ggname.Insecure)
		if err := remote.Write(ref, img, remote.WithAuth(&authn.Basic{Username: "mirror", Password: "upstream-pull-password"})); err != nil {
			t.Fatal(err)
		}
		layers, _ := img.Layers()
		d, _ := layers[0].Digest()
		md, _ := img.Digest()
		return md.String(), d.String()
	}
	appManifest, appLayer := push("payments/app")
	push("billing/app")

	cert, err := f.st.RotateKey(f.ctx, f.admin, "")
	if err != nil {
		t.Fatal(err)
	}
	mirror := registrytest.Mirror(t, upstream, "mirror", "upstream-pull-password", Service, Issuer, cert).Addr
	for _, s := range []Source{
		{Name: "payments", Upstream: upstream, Mirror: mirror, Patterns: []string{"payments/*"}, TeamIDs: []string{f.teamA}},
	} {
		if _, err := f.st.Create(f.ctx, f.admin, s, ""); err != nil {
			t.Fatal(err)
		}
	}
	f.st.Shared = true
	if _, err := f.st.Create(f.ctx, f.admin, Source{Name: "billing", Upstream: upstream, Mirror: mirror, Patterns: []string{"billing/*"}, TeamIDs: []string{f.teamB}}, ""); err != nil {
		t.Fatal(err)
	}
	token := func(team, repo string) string {
		tok, err := f.st.Issue(f.ctx, Request{OrgID: f.org, TeamID: team, Subject: "attempt", Mirror: mirror, Scopes: []string{"repository:" + repo + ":pull"}})
		if err != nil {
			t.Fatal(err)
		}
		return tok.Value
	}
	code := func(path, tok string) int {
		req, _ := http.NewRequest("GET", "http://"+mirror+path, nil)
		req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	a, b := token(f.teamA, "payments/app"), token(f.teamB, "billing/app")
	bOnPayments := token(f.teamB, "payments/app")

	if c := code("/v2/payments/app/manifests/1", a); c != 200 {
		t.Fatalf("team A's own image through the mirror = %d", c)
	}
	if c := code("/v2/payments/app/blobs/"+appLayer, a); c != 200 {
		t.Fatalf("team A's layer = %d", c)
	}
	check := func(t *testing.T, cases map[string][2]any) {
		t.Helper()
		for name, c := range cases {
			got := code(c[0].(string), c[1].(string))
			if got >= 200 && got < 300 {
				t.Errorf("%s = %d", name, got)
			}
		}
	}
	t.Run("SEC-REG-04 cached content is refused to another team", func(t *testing.T) {
		check(t, map[string][2]any{
			"team B on the cached manifest":              {"/v2/payments/app/manifests/1", b},
			"team B with a token requested for payments": {"/v2/payments/app/manifests/1", bOnPayments},
			"team B on the cached layer":                 {"/v2/payments/app/blobs/" + appLayer, b},
			"team B on the manifest by digest":           {"/v2/payments/app/manifests/" + appManifest, b},
			"anonymous":                                  {"/v2/payments/app/manifests/1", ""},
		})
	})
	t.Run("SEC-REG-07 no other paths", func(t *testing.T) {
		if c := code("/v2/billing/app/manifests/1", b); c != 200 {
			t.Fatalf("team B's own image = %d", c)
		}
		check(t, map[string][2]any{
			"team A's layer through team B's repository": {"/v2/billing/app/blobs/" + appLayer, b},
			"catalog with team A's token":                {"/v2/_catalog", a},
			"tag list of payments with team B's token":   {"/v2/payments/app/tags/list", b},
		})
	})
	t.Run("SEC-REG-08 expired and foreign tokens", func(t *testing.T) {
		f.st.now = func() time.Time { return time.Now().Add(-2 * TokenTTL) }
		expired := token(f.teamA, "payments/app")
		f.st.now = time.Now
		other := setup(t)
		if _, err := other.st.RotateKey(other.ctx, other.admin, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := other.st.Create(other.ctx, other.admin, Source{Name: "p", Upstream: upstream, Mirror: mirror, Patterns: []string{"payments/*"}, TeamIDs: []string{other.teamA}}, ""); err != nil {
			t.Fatal(err)
		}
		foreign, _ := other.st.Issue(other.ctx, Request{OrgID: other.org, TeamID: other.teamA, Mirror: mirror, Scopes: []string{"repository:payments/app:pull"}})
		check(t, map[string][2]any{
			"an expired token":                   {"/v2/payments/app/manifests/1", expired},
			"a token signed by an untrusted key": {"/v2/payments/app/manifests/1", foreign.Value},
		})
		list, _ := f.st.List(f.ctx, f.admin)
		for _, s := range list {
			_ = f.st.SetStatus(f.ctx, f.admin, s.ID, false, "")
		}
		if tok, _ := f.st.Issue(f.ctx, Request{OrgID: f.org, TeamID: f.teamA, Mirror: mirror, Scopes: []string{"repository:payments/app:pull"}}); len(tok.Granted) != 0 {
			t.Fatal("a disabled source still grants access")
		}
	})
	t.Run("the cache serves only authorized teams with the upstream down", func(t *testing.T) {
		up.Stop()
		if c := code("/v2/payments/app/manifests/1", a); c != 200 {
			t.Fatalf("team A's cached manifest = %d", c)
		}
		check(t, map[string][2]any{"team B on the cached manifest": {"/v2/payments/app/manifests/1", b}})
	})
}
