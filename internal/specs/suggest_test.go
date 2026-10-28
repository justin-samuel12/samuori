package specs

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/discovery"
)

type suggestionFixture struct {
	Recipes []struct {
		ID               string   `json:"id"`
		Version          string   `json:"version"`
		Languages        []string `json:"languages"`
		ApplicationTypes []string `json:"applicationTypes"`
		Status           string   `json:"status"`
	} `json:"recipes"`
	Cases []struct {
		Name     string              `json:"name"`
		Project  string              `json:"project"`
		Findings []discovery.Finding `json:"findings"`
		Want     []struct {
			App      string `json:"app"`
			Frontend string `json:"frontend"`
			Document any    `json:"document"`
		} `json:"want"`
	} `json:"cases"`
}

func loadSuggestionFixture(t *testing.T) (suggestionFixture, []RecipeChoice) {
	t.Helper()
	raw, err := os.ReadFile("testdata/suggestions.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx suggestionFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	var recipes []RecipeChoice
	for _, r := range fx.Recipes {
		recipes = append(recipes, RecipeChoice{ID: r.ID, Version: r.Version, Languages: r.Languages, ApplicationTypes: r.ApplicationTypes, Approved: r.Status == "approved"})
	}
	return fx, recipes
}

func normalized(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

// TestSuggestMatchesWizard: the server builds the same starting document the wizard builds for an application and
// for a frontend with its backend; ui/src/spec/model.test.ts checks the wizard against the same fixture.
func TestSuggestMatchesWizard(t *testing.T) {
	fx, recipes := loadSuggestionFixture(t)
	for _, c := range fx.Cases {
		find := func(dir string) *discovery.Finding {
			for i := range c.Findings {
				if c.Findings[i].SourceSubpath == dir {
					return &c.Findings[i]
				}
			}
			t.Fatalf("%s: no finding in %s", c.Name, dir)
			return nil
		}
		for _, w := range c.Want {
			f := find(w.App)
			doc := draftForApp(appName(f, c.Project), f, recipes)
			if w.Frontend != "" {
				doc = withFrontend(doc, find(w.Frontend))
			}
			if got := normalized(t, doc); !reflect.DeepEqual(got, w.Document) {
				t.Errorf("%s (%s %s):\n got %v\nwant %v", c.Name, w.App, w.Frontend, got, w.Document)
			}
		}
	}
}

func valid(t *testing.T, doc map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(doc)
	res, err := buildspec.Validate(raw, buildspec.DefaultOptions())
	if err != nil || !res.Valid() {
		t.Fatalf("suggestion is not a valid spec: %v %+v %+v\n%s", err, res.SchemaErrors, res.SemanticErrors, raw)
	}
}

func TestSuggest(t *testing.T) {
	fx, recipes := loadSuggestionFixture(t)
	all := func(string) bool { return true }

	t.Run("a Node.js web service gets a valid recipe spec first", func(t *testing.T) {
		items, _ := Suggest(SuggestInput{Project: "shop", Result: discovery.Result{Findings: fx.Cases[0].Findings}, Recipes: recipes, Allowed: all})
		if len(items) != 3 || items[0].Method != "recipe" || !items[0].Recommended || items[1].Method != "railpack" || items[2].Method != "buildpacks" {
			t.Fatalf("items = %+v", items)
		}
		valid(t, items[0].Document)
		if get(items[0].Document, "spec", "application", "port") != 3000 || items[0].Warnings[len(items[0].Warnings)-1].Code != "port-assumed" {
			t.Errorf("port = %v, warnings %+v", get(items[0].Document, "spec", "application", "port"), items[0].Warnings)
		}
		if get(items[0].Document, "spec", "build", "recipe", "id") != "node-service" {
			t.Errorf("recipe = %v", get(items[0].Document, "spec", "build", "recipe"))
		}
		for _, s := range items[1:] {
			valid(t, s.Document)
		}
	})

	t.Run("a Dockerfile is recommended and builds as written", func(t *testing.T) {
		res := discovery.Result{Findings: fx.Cases[0].Findings, Dockerfiles: []discovery.DockerfileAnalysis{{Path: "Dockerfile", Stages: []string{"build", "1"}, ExposedPorts: []string{"8080"}}}}
		onlyBuiltIn := func(m string) bool { return m == "recipe" || m == "dockerfile" }
		items, _ := Suggest(SuggestInput{Project: "shop", Name: "web", Result: res, Recipes: recipes, Allowed: onlyBuiltIn})
		if len(items) != 2 || items[0].Method != "dockerfile" || !items[0].Recommended || items[1].Method != "recipe" {
			t.Fatalf("items = %+v", items)
		}
		valid(t, items[0].Document)
		d := items[0].Document
		if get(d, "metadata", "name") != "web" || get(d, "spec", "build", "dockerfile", "target") != "1" || get(d, "spec", "application", "port") != 8080 ||
			get(d, "spec", "application", "runtime", "language") != "nodejs" {
			t.Errorf("dockerfile suggestion = %v", d)
		}
	})

	t.Run("an application without an approved recipe is explained, not suggested", func(t *testing.T) {
		f := discovery.Finding{SourceSubpath: ".", Language: "powershell", ApplicationKind: "cli-task"}
		items, notes := Suggest(SuggestInput{Project: "tools", Result: discovery.Result{Findings: []discovery.Finding{f}}, Recipes: recipes, Allowed: func(m string) bool { return m == "recipe" }})
		if len(items) != 0 || len(notes) != 1 || notes[0].Code != "no-recipe" {
			t.Fatalf("items = %+v, notes = %+v", items, notes)
		}
	})
}
