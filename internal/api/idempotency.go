package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/operations"
)

type operationKey struct{}

func routeOperation(ctx context.Context) *openapi3.Operation {
	op, _ := ctx.Value(operationKey{}).(*openapi3.Operation)
	return op
}

// optionalIdempotencyKey reports whether the operation declares Idempotency-Key as optional. Operations that require
// it handle the key in their own handler.
func optionalIdempotencyKey(op *openapi3.Operation) bool {
	if op == nil {
		return false
	}
	for _, p := range op.Parameters {
		if p.Value != nil && p.Value.In == openapi3.ParameterInHeader && http.CanonicalHeaderKey(p.Value.Name) == "Idempotency-Key" {
			return !p.Value.Required
		}
	}
	return false
}

// replayCreates gives creates that declare an optional Idempotency-Key the shared operations record: the first
// request runs and records the created id; a repeat answers 409 idempotent_replay without running again. A key whose
// first request was rejected with a 4xx is released, so the corrected request can reuse it.
func (a *API) replayCreates(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		op := routeOperation(r.Context())
		if key == "" || !optionalIdempotencyKey(op) {
			next.ServeHTTP(w, r)
			return
		}
		p := principal(r)
		if p.ID == "" {
			next.ServeHTTP(w, r)
			return
		}
		kind := "op:" + op.OperationID
		var rec operations.Operation
		var created bool
		if err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
			var err error
			rec, created, err = a.ops.Begin(r.Context(), tx, p, kind, key)
			return err
		}); err != nil {
			a.internalError(w, r, err)
			return
		}
		if !created {
			switch rec.Status {
			case operations.Succeeded:
				msg := "a request with this idempotency key already succeeded; it was not repeated"
				if rec.ResourceID != "" {
					msg = "a request with this idempotency key already created " + rec.ResourceID + "; it was not repeated"
				}
				writeReplay(w, rec.ResourceID, msg)
			default:
				apigen.WriteProblem(w, http.StatusConflict, "in_progress", "a request with this idempotency key did not complete; use a new key")
			}
			return
		}
		cw := &captureWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(cw, r)
		ctx := context.WithoutCancel(r.Context())
		err := a.db.InTx(ctx, func(tx *sql.Tx) error {
			switch {
			case cw.status >= 200 && cw.status < 300:
				return a.ops.Finish(ctx, tx, rec.ID, operations.Succeeded, kind, cw.createdID(), "", "")
			case cw.status >= 400 && cw.status < 500:
				return a.ops.Discard(ctx, tx, rec.ID)
			default:
				return a.ops.Finish(ctx, tx, rec.ID, operations.Failed, kind, "", "failed", "")
			}
		})
		if err != nil {
			a.log.Error("finishing operation", "operation", rec.ID, "error", err)
		}
	})
}

func writeReplay(w http.ResponseWriter, resourceID, message string) {
	problem := apigen.Problem{Code: "idempotent_replay", Message: message, RequestId: w.Header().Get("X-Request-ID")}
	if resourceID != "" {
		problem.Details = &[]apigen.ProblemDetail{{Field: ptr("id"), Code: ptr("created"), Reason: ptr(resourceID)}}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(problem)
}

const captureLimit = 64 << 10

// captureWriter passes the response through and keeps the start of a successful body to read the created id.
type captureWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func (c *captureWriter) WriteHeader(status int) {
	if !c.wroteHeader {
		c.status, c.wroteHeader = status, true
	}
	c.ResponseWriter.WriteHeader(status)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	c.wroteHeader = true
	if c.status < 300 && c.body.Len() < captureLimit {
		c.body.Write(b[:min(len(b), captureLimit-c.body.Len())])
	}
	return c.ResponseWriter.Write(b)
}

func (c *captureWriter) createdID() string {
	var v struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(c.body.Bytes(), &v) != nil {
		return ""
	}
	return v.ID
}
