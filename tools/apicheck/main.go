// Command apicheck validates the OpenAPI document and the Samuori API rules in docs/architecture/api.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/getkin/kin-openapi/openapi3"
)

var (
	sensitiveName = regexp.MustCompile(`(?i)^(password|passwd|secret|secretvalue|value|token|accesstoken|refreshtoken|sessiontoken|privatekey|clientsecret|accesskey|secretaccesskey|credential)$`)
	workerPath    = regexp.MustCompile(`(?i)(\blease|heartbeat|secret-release|plan-fetch)`)
	signedOps     = map[string]bool{"receivePipelineHook": true}
	revealOnce    = map[string]bool{"ApiTokenIssued": true, "EnrollmentTokenIssued": true, "PipelineCreated": true, "PipelineSecretIssued": true, "ControllerJoinTokenIssued": true}
	needsIdem     = map[string]bool{
		"createBuild": true, "cancelBuild": true, "requestPublication": true, "decideApproval": true,
		"createEnrollmentToken": true, "createSecretBinding": true, "applySecretBindingAction": true,
		"storeLocalSecret": true, "commitUpload": true, "applyWorkerAction": true, "startPipelineRun": true,
	}
)

func main() {
	path := flag.String("spec", "api/openapi/samuori.v1.yml", "OpenAPI document")
	protoPath := flag.String("proto", "", "worker protocol .proto file")
	flag.Parse()
	doc, problems, err := check(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "apicheck:", err)
		os.Exit(1)
	}
	if *protoPath != "" {
		pp, err := checkProto(*protoPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "apicheck:", err)
			os.Exit(1)
		}
		fmt.Printf("proto %s problems=%d\n", filepath.Base(*protoPath), len(pp))
		problems = append(problems, pp...)
	}
	for _, p := range problems {
		fmt.Println("FAIL ", p)
	}
	ops := 0
	for _, item := range doc.Paths.Map() {
		ops += len(item.Operations())
	}
	fmt.Printf("paths=%d operations=%d schemas=%d problems=%d\n", doc.Paths.Len(), ops, len(doc.Components.Schemas), len(problems))
	if len(problems) > 0 {
		os.Exit(1)
	}
}

func check(path string) (*openapi3.T, []string, error) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(path)
	if err != nil {
		return nil, nil, err
	}
	if err := doc.Validate(context.Background(), openapi3.EnableExamplesValidation()); err != nil {
		return doc, []string{"openapi: " + err.Error()}, nil
	}
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	for p, item := range doc.Paths.Map() {
		if workerPath.MatchString(p) {
			add("%s: worker protocol endpoints do not belong in the public API", p)
		}
		for method, op := range item.Operations() {
			where := method + " " + p
			public := op.Extensions["x-samuori-public"] == true
			if op.OperationID == "" {
				add("%s: missing operationId", where)
			}
			if op.Extensions["x-samuori-signed"] == true && (!public || !signedOps[op.OperationID]) {
				add("%s: x-samuori-signed is only for approved public operations verified by signature", where)
			}
			if public {
				if op.Security == nil || len(*op.Security) != 0 {
					add("%s: public operation must declare empty security explicitly", where)
				}
				continue
			}
			if roles, _ := op.Extensions["x-samuori-roles"].([]any); len(roles) == 0 {
				add("%s: missing x-samuori-roles", where)
			}
			if op.Security != nil && len(*op.Security) == 0 {
				add("%s: unauthenticated without x-samuori-public", where)
			}
			params := paramNames(item.Parameters, op.Parameters)
			if method != "GET" && !params["header:X-CSRF-Token"] {
				add("%s: unsafe method without X-CSRF-Token parameter", where)
			}
			if needsIdem[op.OperationID] && !params["header:Idempotency-Key"] {
				add("%s: missing Idempotency-Key", where)
			}
		}
	}

	names := make([]string, 0, len(doc.Components.Schemas))
	for n := range doc.Components.Schemas {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		walk(n, n, doc.Components.Schemas[n].Value, map[*openapi3.Schema]bool{}, add)
	}
	sort.Strings(problems)
	return doc, problems, nil
}

func walk(root, at string, s *openapi3.Schema, seen map[*openapi3.Schema]bool, add func(string, ...any)) {
	if s == nil || seen[s] {
		return
	}
	seen[s] = true
	for name, ref := range s.Properties {
		p := ref.Value
		if p != nil && sensitiveName.MatchString(name) && !p.WriteOnly {
			if p.Extensions["x-samuori-reveal-once"] != true || !revealOnce[root] {
				add("schema %s.%s: credential-like field must be writeOnly (or an approved reveal-once field)", at, name)
			}
		}
		walk(root, at+"."+name, p, seen, add)
	}
	for _, sub := range s.AllOf {
		walk(root, at, sub.Value, seen, add)
	}
	if s.Items != nil {
		walk(root, at+"[]", s.Items.Value, seen, add)
	}
}

func paramNames(lists ...openapi3.Parameters) map[string]bool {
	out := map[string]bool{}
	for _, l := range lists {
		for _, p := range l {
			if p.Value != nil {
				out[p.Value.In+":"+p.Value.Name] = true
			}
		}
	}
	return out
}
