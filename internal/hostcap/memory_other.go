//go:build !linux

package hostcap

func memory() (total, available uint64) { return 0, 0 }
