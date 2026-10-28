package kubeconvert

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

var printer = message.NewPrinter(language.English)

// The schemas of the kinds Samuori validates, extracted by tools/kubeschema from Kubernetes v1.36.5's OpenAPI document.
//
//go:embed schemas/kubernetes-v1.36.json
var kubeSchemas []byte

const schemaURL = "https://samuori.invalid/schemas/kubernetes-v1.36.json"

type schemaSet struct {
	once     sync.Once
	err      error
	kinds    map[string]string
	compiled map[string]*jsonschema.Schema
}

var schemas schemaSet

func (s *schemaSet) load() error {
	s.once.Do(func() {
		var doc struct {
			Kinds map[string]string `json:"kinds"`
		}
		if s.err = json.Unmarshal(kubeSchemas, &doc); s.err != nil {
			return
		}
		raw, err := jsonschema.UnmarshalJSON(bytes.NewReader(kubeSchemas))
		if err != nil {
			s.err = err
			return
		}
		c := jsonschema.NewCompiler()
		if s.err = c.AddResource(schemaURL, raw); s.err != nil {
			return
		}
		s.kinds, s.compiled = doc.Kinds, map[string]*jsonschema.Schema{}
		for gvk, def := range doc.Kinds {
			sch, err := c.Compile(schemaURL + "#/definitions/" + def)
			if err != nil {
				s.err = fmt.Errorf("%s: %w", gvk, err)
				return
			}
			s.compiled[gvk] = sch
		}
	})
	return s.err
}

var errUnknownKind = errors.New("kind not in Samuori's schema set")

// ValidateObject checks one Kubernetes object against Samuori's embedded schemas and returns each violation.
func ValidateObject(obj map[string]any) ([]string, error) { return validateObject(obj) }

// validateObject checks one object against the schema of its apiVersion and kind and returns each violation as
// "field: message".
func validateObject(obj map[string]any) ([]string, error) {
	if err := schemas.load(); err != nil {
		return nil, err
	}
	apiVersion, _ := obj["apiVersion"].(string)
	kind, _ := obj["kind"].(string)
	sch, ok := schemas.compiled[apiVersion+"/"+kind]
	if !ok {
		return nil, errUnknownKind
	}
	js, err := json.Marshal(obj)
	if err != nil {
		return []string{"the object cannot be represented as JSON: " + err.Error()}, nil
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
	if err != nil {
		return nil, err
	}
	err = sch.Validate(inst)
	if err == nil {
		return nil, nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			field := strings.Join(e.InstanceLocation, ".")
			if field == "" {
				field = "(object)"
			}
			msg := field + ": " + e.ErrorKind.LocalizedString(printer)
			if !seen[msg] {
				seen[msg] = true
				out = append(out, msg)
			}
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	sort.Strings(out)
	return out, nil
}
