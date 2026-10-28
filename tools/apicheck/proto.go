package main

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/reflect/protoreflect"
)

var (
	sensitiveProtoField = regexp.MustCompile(`(?i)^(value|token|secret|password|private_key|enrollment_token|credential)$`)
	// Mirror tokens are short-lived, repository-scoped pull grants for BuildKit's resolver (ADR 0030).
	allowedSensitive = map[string]bool{"SecretValue.value": true, "ReleaseSecretResponse.secret": true, "EnrollRequest.enrollment_token": true,
		"MirrorToken.token": true, "MirrorTokenResponse.token": true}
	leaseBound          = map[string]bool{"GetPlanRequest": true, "ReleaseSecretRequest": true, "MirrorTokenRequest": true, "AttemptReport": true, "ArtifactUploadRequest": true, "GetSourceRequest": true, "GetCacheRequest": true}
	assignmentForbidden = map[string]bool{"SecretValue": true, "GetPlanResponse": true}
)

func checkProto(path string) ([]string, error) {
	c := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: []string{filepath.Dir(path)}}),
	}
	files, err := c.Compile(context.Background(), filepath.Base(path))
	if err != nil {
		return []string{"compile: " + err.Error()}, nil
	}
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	fd := files[0]
	if !regexp.MustCompile(`\.v[0-9]+$`).MatchString(string(fd.Package())) {
		add("package %s must end with a version segment", fd.Package())
	}
	msgs := fd.Messages()
	for i := 0; i < msgs.Len(); i++ {
		m := msgs.Get(i)
		name := string(m.Name())
		fields := m.Fields()
		hasLease := false
		for j := 0; j < fields.Len(); j++ {
			f := fields.Get(j)
			qualified := name + "." + string(f.Name())
			if sensitiveProtoField.MatchString(string(f.Name())) && !allowedSensitive[qualified] {
				add("%s: secret-like field outside the approved messages", qualified)
			}
			if f.Name() == "lease" && f.Message() != nil && f.Message().Name() == "LeaseRef" {
				hasLease = true
			}
		}
		if leaseBound[name] && !hasLease {
			add("%s: lease-bound request must carry a LeaseRef named lease", name)
		}
		if name == "Assignment" {
			for _, r := range reachable(m, map[protoreflect.FullName]bool{}) {
				if assignmentForbidden[r] {
					add("Assignment reaches %s: assignments must not carry secrets or plan content", r)
				}
			}
		}
	}
	sort.Strings(problems)
	return problems, nil
}

func reachable(m protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool) []string {
	if seen[m.FullName()] {
		return nil
	}
	seen[m.FullName()] = true
	var out []string
	fields := m.Fields()
	for i := 0; i < fields.Len(); i++ {
		if sub := fields.Get(i).Message(); sub != nil {
			out = append(out, string(sub.Name()))
			out = append(out, reachable(sub, seen)...)
		}
	}
	return out
}
