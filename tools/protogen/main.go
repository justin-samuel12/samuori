// Command protogen compiles api/proto with protocompile and runs the pinned protoc-gen-go and protoc-gen-go-grpc
// plugins, so generating Go stubs needs no system protoc.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

func main() {
	root := flag.String("root", "../../api/proto", "proto import root")
	file := flag.String("file", "samuori/worker/v1/worker.proto", "proto file relative to root")
	out := flag.String("out", "../..", "directory that go_package paths are relative to")
	module := flag.String("module", "samuori.invalid/samuori", "module prefix stripped from go_package")
	flag.Parse()
	if err := run(*root, *file, *out, *module); err != nil {
		fmt.Fprintln(os.Stderr, "protogen:", err)
		os.Exit(1)
	}
}

func run(root, file, out, module string) error {
	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: []string{root}})}
	files, err := c.Compile(context.Background(), file)
	if err != nil {
		return err
	}
	var protos []*descriptorpb.FileDescriptorProto
	seen := map[string]bool{}
	var add func(fd protoreflect.FileDescriptor)
	add = func(fd protoreflect.FileDescriptor) {
		if seen[fd.Path()] {
			return
		}
		seen[fd.Path()] = true
		imports := fd.Imports()
		for i := 0; i < imports.Len(); i++ {
			add(imports.Get(i).FileDescriptor)
		}
		protos = append(protos, protodesc.ToFileDescriptorProto(fd))
	}
	add(files[0])
	req := &pluginpb.CodeGeneratorRequest{FileToGenerate: []string{file}, ProtoFile: protos, Parameter: proto.String("paths=import")}
	for _, plugin := range []string{"protoc-gen-go", "protoc-gen-go-grpc"} {
		in, err := proto.Marshal(req)
		if err != nil {
			return err
		}
		cmd := exec.Command("go", "tool", plugin)
		cmd.Stdin, cmd.Stderr = bytes.NewReader(in), os.Stderr
		raw, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("%s: %w", plugin, err)
		}
		var resp pluginpb.CodeGeneratorResponse
		if err := proto.Unmarshal(raw, &resp); err != nil {
			return err
		}
		if resp.Error != nil {
			return fmt.Errorf("%s: %s", plugin, resp.GetError())
		}
		for _, f := range resp.File {
			rel := strings.TrimPrefix(f.GetName(), module+"/")
			dst := filepath.Join(out, rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(dst, []byte(f.GetContent()), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}
