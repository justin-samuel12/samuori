//go:build !linux

package runtimeprobe

func run(Spec) Result {
	return Result{Outcome: "errored", Reason: "runtime tests run only on Linux workers"}
}

func Confine() (Constraints, error) {
	return Constraints{}, errSandbox
}
