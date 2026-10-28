// Command natsoutbox is the M0 NATS/outbox spike. See docs/spikes/nats-outbox.md.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	_ "modernc.org/sqlite"
)

const (
	port    = 14222
	stream  = "JOBS"
	subject = "jobs.queued"
	ackWait = 2 * time.Second
)

var dir string

func main() {
	if len(os.Args) < 2 {
		fail("usage: natsoutbox <scenarios|enqueue|relay|consume|reconcile> [flags]")
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.StringVar(&dir, "dir", "", "data directory")
	n := fs.Int("n", 1, "jobs to enqueue")
	crash := fs.String("crash", "", "crash point")
	must(fs.Parse(os.Args[2:]))
	switch cmd {
	case "scenarios":
		scenarios()
	case "enqueue":
		enqueue(*n, *crash)
	case "relay":
		withJS(func(js jetstream.JetStream) { relay(js, *crash) })
	case "consume":
		withJS(func(js jetstream.JetStream) { consume(js, *crash) })
	case "reconcile":
		reconcile()
	default:
		fail("unknown command " + cmd)
	}
}

func db() *sql.DB {
	d, err := sql.Open("sqlite", filepath.Join(dir, "samuori.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	must(err)
	_, err = d.Exec(`
CREATE TABLE IF NOT EXISTS jobs (id TEXT PRIMARY KEY, state TEXT NOT NULL, version INTEGER NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS outbox (seq INTEGER PRIMARY KEY AUTOINCREMENT, msg_id TEXT UNIQUE NOT NULL, job_id TEXT NOT NULL, published_at TEXT);
CREATE TABLE IF NOT EXISTS transitions (job_id TEXT NOT NULL, from_state TEXT NOT NULL, to_state TEXT NOT NULL, msg_id TEXT NOT NULL);`)
	must(err)
	return d
}

func withJS(fn func(jetstream.JetStream)) {
	ns, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: port, JetStream: true, StoreDir: filepath.Join(dir, "js"), NoSigs: true, NoLog: true})
	must(err)
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		fail("nats not ready")
	}
	nc, err := nats.Connect(ns.ClientURL())
	must(err)
	js, err := jetstream.New(nc)
	must(err)
	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{Name: stream, Subjects: []string{"jobs.>"}, Storage: jetstream.FileStorage, Duplicates: 2 * time.Minute})
	must(err)
	fn(js)
	nc.Close()
	ns.Shutdown()
	ns.WaitForShutdown()
}

func enqueue(n int, crash string) {
	d := db()
	tx, err := d.Begin()
	must(err)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("job-%d-%d", time.Now().UnixNano(), i)
		_, err = tx.Exec(`INSERT INTO jobs VALUES (?, 'queued', 1, ?)`, id, now())
		must(err)
		_, err = tx.Exec(`INSERT INTO outbox (msg_id, job_id) VALUES (?, ?)`, id+":1", id)
		must(err)
	}
	must(tx.Commit())
	if crash == "after-commit" {
		os.Exit(3)
	}
}

func relay(js jetstream.JetStream, crash string) {
	d := db()
	rows, err := d.Query(`SELECT msg_id, job_id FROM outbox WHERE published_at IS NULL ORDER BY seq`)
	must(err)
	type row struct{ msg, job string }
	var pending []row
	for rows.Next() {
		var r row
		must(rows.Scan(&r.msg, &r.job))
		pending = append(pending, r)
	}
	must(rows.Close())
	dups := 0
	for _, r := range pending {
		ack, err := js.Publish(context.Background(), subject, []byte(r.job), jetstream.WithMsgID(r.msg))
		must(err)
		if ack.Duplicate {
			dups++
		}
	}
	fmt.Printf("relay: published=%d duplicates-dropped-by-jetstream=%d\n", len(pending), dups)
	if crash == "after-publish" {
		os.Exit(3)
	}
	for _, r := range pending {
		_, err := d.Exec(`UPDATE outbox SET published_at = ? WHERE msg_id = ?`, now(), r.msg)
		must(err)
	}
}

func consume(js jetstream.JetStream, crash string) {
	ctx := context.Background()
	cons, err := js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{Durable: "scheduler", AckPolicy: jetstream.AckExplicitPolicy, AckWait: ackWait})
	must(err)
	d := db()
	handled, noops := 0, 0
	for {
		batch, err := cons.Fetch(50, jetstream.FetchMaxWait(3*ackWait))
		must(err)
		got := 0
		for m := range batch.Messages() {
			got++
			job := string(m.Data())
			msgID := m.Headers().Get(jetstream.MsgIDHeader)
			tx, err := d.Begin()
			must(err)
			res, err := tx.Exec(`UPDATE jobs SET state='assigned', version=version+1, updated_at=? WHERE id=? AND state='queued'`, now(), job)
			must(err)
			if c, _ := res.RowsAffected(); c == 1 {
				_, err = tx.Exec(`INSERT INTO transitions VALUES (?, 'queued', 'assigned', ?)`, job, msgID)
				must(err)
				handled++
			} else {
				noops++
			}
			must(tx.Commit())
			if crash == "before-ack" {
				fmt.Printf("consume: crashing before ack after %d messages\n", got)
				os.Exit(3)
			}
			must(m.Ack())
		}
		if got == 0 {
			break
		}
	}
	info, err := cons.Info(ctx)
	must(err)
	fmt.Printf("consume: transitions=%d idempotent-noops=%d pending=%d ack-pending=%d\n", handled, noops, info.NumPending, info.NumAckPending)
}

func reconcile() {
	d := db()
	res, err := d.Exec(`INSERT INTO outbox (msg_id, job_id)
SELECT j.id || ':r' || j.version || '-' || ?, j.id FROM jobs j WHERE j.state = 'queued'`, time.Now().UnixNano())
	must(err)
	c, _ := res.RowsAffected()
	fmt.Printf("reconcile: re-enqueued=%d\n", c)
}

func scenarios() {
	self, err := os.Executable()
	must(err)
	run := func(label string, args ...string) {
		cmd := exec.Command(self, args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err := cmd.Run()
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee):
			fmt.Printf("  [%s] process exited with code %d\n", label, ee.ExitCode())
		case err != nil:
			fail(err.Error())
		}
	}
	report := func(base string) {
		dir = base
		d := db()
		var queued, assigned, transitions, dupTransitions int
		must(d.QueryRow(`SELECT COUNT(*) FROM jobs WHERE state='queued'`).Scan(&queued))
		must(d.QueryRow(`SELECT COUNT(*) FROM jobs WHERE state='assigned'`).Scan(&assigned))
		must(d.QueryRow(`SELECT COUNT(*) FROM transitions`).Scan(&transitions))
		must(d.QueryRow(`SELECT COUNT(*) FROM (SELECT job_id FROM transitions GROUP BY job_id HAVING COUNT(*) > 1)`).Scan(&dupTransitions))
		fmt.Printf("  RESULT queued=%d assigned=%d transitions=%d jobs-with-duplicate-transitions=%d\n", queued, assigned, transitions, dupTransitions)
	}
	tmp, err := os.MkdirTemp("", "natsoutbox")
	must(err)
	defer os.RemoveAll(tmp)
	scenario := func(name string, steps ...[]string) {
		base := filepath.Join(tmp, strconv.Itoa(len(name))+name)
		must(os.MkdirAll(base, 0o700))
		fmt.Printf("\n== %s\n", name)
		for _, s := range steps {
			run(s[0], append([]string{s[0], "-dir", base}, s[1:]...)...)
		}
		report(base)
	}

	scenario("S1 crash after DB commit, before publish",
		[]string{"enqueue", "-n", "5", "-crash", "after-commit"},
		[]string{"relay"},
		[]string{"consume"})
	scenario("S2 crash after publish, before outbox marked; relay retries",
		[]string{"enqueue", "-n", "3"},
		[]string{"relay", "-crash", "after-publish"},
		[]string{"relay"},
		[]string{"consume"})
	scenario("S3 consumer crash after DB update, before ack; redelivery",
		[]string{"enqueue", "-n", "2"},
		[]string{"relay"},
		[]string{"consume", "-crash", "before-ack"},
		[]string{"consume"})
	scenario("S4 NATS restart between every step (file storage)",
		[]string{"enqueue", "-n", "4"},
		[]string{"relay"},
		[]string{"consume"},
		[]string{"consume"})

	base := filepath.Join(tmp, "s5")
	must(os.MkdirAll(base, 0o700))
	fmt.Printf("\n== S5 JetStream store lost; reconciliation recovers from the DB\n")
	run("enqueue", "enqueue", "-dir", base, "-n", "3")
	run("relay", "relay", "-dir", base)
	must(os.RemoveAll(filepath.Join(base, "js")))
	fmt.Println("  deleted JetStream store")
	run("consume", "consume", "-dir", base)
	run("reconcile", "reconcile", "-dir", base)
	run("relay", "relay", "-dir", base)
	run("consume", "consume", "-dir", base)
	report(base)
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func must(err error) {
	if err != nil {
		fail(err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "natsoutbox:", msg)
	os.Exit(1)
}
