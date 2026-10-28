package api

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestCapacity: administrators see every host's last report, unfinished uploads and builds in one view; workers'
// reports are cleaned on the way out; developers are refused.
func TestCapacity(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, _, projA, _ := twoTeams(t, l)
	admin := l.adminMember(t)
	worker := enrollConversionWorker(t, l)
	report := `{"cpus":4,"memoryTotalBytes":8000,"memoryAvailableBytes":9000,"volumes":[{"label":"Builds\u001b[31m","path":"/var/lib/samuori-worker","totalBytes":100,"freeBytes":40}]}`
	if _, err := l.db.SQL.Exec(l.db.Rebind(`UPDATE workers SET capacity = ?, capacity_at = ? WHERE id = ?`), report, time.Now().UTC(), worker); err != nil {
		t.Fatal(err)
	}
	if r := l.call(t, alice.c, "POST", "/projects/"+projA+"/uploads", `{"fileCount":1,"totalBytes":10}`, alice.csrf); r.StatusCode != 201 {
		t.Fatalf("upload = %d", r.StatusCode)
	}
	if code, _, b := l.json(t, alice, "GET", "/system/capacity", "", nil); code != 403 {
		t.Errorf("developer = %d %v", code, b)
	}
	code, _, b := l.json(t, admin, "GET", "/system/capacity", "", nil)
	if code != 200 {
		t.Fatalf("capacity = %d %v", code, b)
	}
	got := fmt.Sprint(b["workers"])
	if !strings.Contains(got, "name:w1") || !strings.Contains(got, "memoryAvailableBytes:8000") || !strings.Contains(got, "label:Builds[31m") || strings.ContainsRune(got, 0x1b) {
		t.Errorf("workers = %s", got)
	}
	if up := fmt.Sprint(b["uploads"]); !strings.Contains(up, "status:open") || !strings.Contains(up, "createdBy:alice") {
		t.Errorf("uploads = %s", up)
	}
	if len(b["controllers"].([]any)) == 0 {
		t.Errorf("no controllers: %v", b)
	}
}
