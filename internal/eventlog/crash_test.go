package eventlog

// SL-5 (durability): every append acknowledged before a SIGKILL must survive it.
// The test re-executes this test binary as a helper process that appends in a
// loop and prints each acknowledged seq; the parent kills it mid-stream and
// verifies the database contains every acknowledged event.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/korya/creo/internal/store"
)

func TestMain(m *testing.M) {
	if dbPath := os.Getenv("CREO_CRASH_HELPER_DB"); dbPath != "" {
		crashHelper(dbPath)
		return
	}
	os.Exit(m.Run())
}

func crashHelper(dbPath string) {
	db, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	l := New(db)
	ctx := context.Background()
	out := bufio.NewWriter(os.Stdout)
	for i := 0; ; i++ {
		seq, err := l.Append(ctx, "s1", []NewEvent{{Type: "crash", UserText: strconv.Itoa(i)}}, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintf(out, "%d\n", seq)
		out.Flush() // seq is only reported after commit; report = acknowledgement
	}
}

func TestCrashDurability(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess test")
	}
	dbPath := filepath.Join(t.TempDir(), "crash.db")
	{
		db, err := store.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		seedSession(t, db, "s1")
		db.Close()
	}

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "CREO_CRASH_HELPER_DB="+dbPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Read acknowledged seqs until we have a decent sample, then SIGKILL.
	var lastAcked int64
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		seq, err := strconv.ParseInt(sc.Text(), 10, 64)
		if err != nil {
			t.Fatalf("bad ack line %q", sc.Text())
		}
		lastAcked = seq
		if lastAcked >= 50 {
			break
		}
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen after kill: %v", err)
	}
	defer db.Close()
	l := New(db)
	evs, err := l.Read(context.Background(), "s1", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(evs)) < lastAcked {
		t.Fatalf("SL-5 violated: %d acks, only %d events survived", lastAcked, len(evs))
	}
	for i := int64(0); i < lastAcked; i++ {
		if evs[i].Seq != i+1 {
			t.Fatalf("gap after crash at seq %d", i+1)
		}
	}
}
