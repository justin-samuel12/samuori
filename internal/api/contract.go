package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"

	"samuori.invalid/samuori/api/openapi"
)

type contractDoc struct {
	body []byte
	etag string
}

func newContractDoc(b []byte) contractDoc {
	sum := sha256.Sum256(b)
	return contractDoc{body: b, etag: `"` + hex.EncodeToString(sum[:16]) + `"`}
}

var contractYAML = newContractDoc(openapi.Document)

var contractJSON = sync.OnceValues(func() (contractDoc, error) {
	// Loaded from the YAML rather than apigen.GetSwagger, whose embedded copy renames operation IDs.
	spec, err := openapi3.NewLoader().LoadFromData(openapi.Document)
	if err != nil {
		return contractDoc{}, err
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return contractDoc{}, err
	}
	return newContractDoc(b), nil
})

func (a *API) GetApiContract(w http.ResponseWriter, r *http.Request) {
	serveContract(w, r, contractYAML, "application/yaml; charset=utf-8")
}

func (a *API) GetApiContractJson(w http.ResponseWriter, r *http.Request) {
	doc, err := contractJSON()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	serveContract(w, r, doc, "application/json")
}

// The contract is only served to signed-in callers, so shared caches must not keep it (ADR 0066).
func serveContract(w http.ResponseWriter, r *http.Request, doc contractDoc, contentType string) {
	w.Header().Set("ETag", doc.etag)
	w.Header().Set("Cache-Control", "private, max-age=300")
	if r.Header.Get("If-None-Match") == doc.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(doc.body)
}
