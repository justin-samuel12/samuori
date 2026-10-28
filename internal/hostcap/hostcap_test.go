package hostcap

import (
	"strings"
	"testing"
)

// TestMeasureOncePerFilesystem: directories on the same filesystem become one volume with both labels.
func TestMeasureOncePerFilesystem(t *testing.T) {
	dir := t.TempDir()
	r := Measure([]Dir{{"Builds", dir}, {"Updates", dir}, {"Missing", dir + "/nope"}})
	if len(r.Volumes) != 1 || r.Volumes[0].Label != "Builds, Updates" || r.Volumes[0].TotalBytes == 0 || r.Volumes[0].FreeBytes > r.Volumes[0].TotalBytes {
		t.Fatalf("report = %+v", r)
	}
	if r.CPUs < 1 {
		t.Errorf("cpus = %d", r.CPUs)
	}
}

// TestClean: a worker's report is bounded before it is stored.
func TestClean(t *testing.T) {
	in := Report{CPUs: -3, MemoryTotalBytes: 10, MemoryAvailableBytes: 50}
	for range 12 {
		in.Volumes = append(in.Volumes, Volume{Label: "a\x1b[31m" + strings.Repeat("x", 300), Path: "/p", TotalBytes: 5, FreeBytes: 9})
	}
	in.Volumes = append([]Volume{{Label: "empty"}}, in.Volumes...)
	r := Clean(in)
	if r.CPUs != 0 || r.MemoryAvailableBytes != 10 || len(r.Volumes) != MaxVolumes {
		t.Fatalf("report = %+v", r)
	}
	v := r.Volumes[0]
	if v.FreeBytes != 5 || len(v.Label) > maxLabelLen || strings.ContainsRune(v.Label, 0x1b) {
		t.Errorf("volume = %+v", v)
	}
}
