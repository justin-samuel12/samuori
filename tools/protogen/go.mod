module samuori.invalid/samuori/tools/protogen

go 1.27.0

toolchain go1.27.2

require (
	github.com/bufbuild/protocompile v0.14.1
	google.golang.org/protobuf v1.36.12
)

require (
	golang.org/x/sync v0.8.0 // indirect
	google.golang.org/grpc/cmd/protoc-gen-go-grpc v1.6.2 // indirect
)

tool (
	google.golang.org/grpc/cmd/protoc-gen-go-grpc
	google.golang.org/protobuf/cmd/protoc-gen-go
)
