package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func awaitSSH(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("SSH fixture condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSSHRecoveryBeforeExecution(t *testing.T) {
	b, dir := fakeTransportSSH(t)
	t.Setenv("FAKE_SSH_REFUSE_FIRST", "2")
	if _, err := b.Mkdir(context.Background(), "Recovered", ""); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if strings.Count(string(calls), "call\n") != 3 {
		t.Fatalf("attempts: %s", calls)
	}
	docs, err := b.List(context.Background())
	if err != nil || len(docs) != 1 {
		t.Fatalf("docs=%v err=%v", docs, err)
	}
	if got := b.Diagnostics()["retries"]; got != uint64(2) {
		t.Fatal(got)
	}
}

func TestSSHCancelledQueueAndFIFO(t *testing.T) {
	b, dir := fakeTransportSSH(t)
	transportFixture(t, dir, "block", nil)
	first := make(chan error, 1)
	go func() { _, err := b.Mkdir(context.Background(), "first", ""); first <- err }()
	awaitSSH(t, func() bool { _, err := os.Stat(filepath.Join(dir, "started")); return err == nil })
	ctx, cancel := context.WithCancel(context.Background())
	queued := make(chan error, 1)
	go func() { _, err := b.Mkdir(ctx, "cancelled", ""); queued <- err }()
	awaitSSH(t, func() bool { return b.Diagnostics()["queue_depth"] == 1 })
	cancel()
	if err := <-queued; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for i, name := range []string{"second", "third"} {
		go func(name string) { _, err := b.Mkdir(context.Background(), name, ""); results <- err }(name)
		depth := i + 1
		awaitSSH(t, func() bool { return b.Diagnostics()["queue_depth"] == depth })
	}
	if err := os.Remove(filepath.Join(dir, "block")); err != nil {
		t.Fatal(err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	docs, err := b.List(context.Background())
	if err != nil || len(docs) != 3 {
		t.Fatalf("%v %v", docs, err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if strings.Count(string(calls), "call\n") != 4 {
		t.Fatalf("cancelled job executed: %s", calls)
	}
	commands, _ := os.ReadFile(filepath.Join(dir, "commands"))
	previous := -1
	for _, name := range []string{"first", "second", "third"} {
		id := ""
		for _, d := range docs {
			if d.Name == name {
				id = d.ID
			}
		}
		index := strings.Index(string(commands), id+".metadata")
		if id == "" || index <= previous {
			t.Fatalf("FIFO order lost for %q: %s", name, commands)
		}
		previous = index
	}
}

func TestSSHActiveCancellationAndCloseDrain(t *testing.T) {
	b, dir := fakeTransportSSH(t)
	transportFixture(t, dir, "block", nil)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := b.Mkdir(ctx, "active", ""); result <- err }()
	awaitSSH(t, func() bool { _, err := os.Stat(filepath.Join(dir, "started")); return err == nil })
	cancel()
	err := <-result
	var unknown *SSHExecutionUnknownError
	if !errors.As(err, &unknown) || !errors.Is(err, context.Canceled) {
		t.Fatalf("active cancel: %T %v", err, err)
	}
	if !b.RefreshPending() {
		t.Fatal("lost unknown mutation")
	}
	if err := os.Remove(filepath.Join(dir, "block")); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.List(context.Background()); !errors.Is(err, ErrSSHClosed) {
		t.Fatal(err)
	}
	if err := b.RefreshTablet(context.Background()); !errors.Is(err, ErrSSHClosed) {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if string(calls) != "call\n" {
		t.Fatalf("cancelled write replayed: %s", calls)
	}
}

func TestSSHConcurrentWritesAndExplicitRefreshCoalesce(t *testing.T) {
	b, dir := fakeTransportSSH(t)
	t.Setenv("REMARKABLE_DEFER_RESTART", "")
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := b.Mkdir(context.Background(), "batch", ""); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	log, _ := os.ReadFile(filepath.Join(dir, "systemctl-calls"))
	if strings.Count(string(log), "restart\n") != 1 {
		t.Fatalf("not coalesced: %s", log)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if err := b.RefreshTablet(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if string(before) != string(after) {
		t.Fatal("known-clean explicit refresh executed commands")
	}
	t.Setenv("REMARKABLE_DEFER_RESTART", "1")
	if _, err := b.Mkdir(context.Background(), "deferred", ""); err != nil {
		t.Fatal(err)
	}
	errs = make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- b.RefreshTablet(context.Background()) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	log, _ = os.ReadFile(filepath.Join(dir, "systemctl-calls"))
	if strings.Count(string(log), "restart\n") != 2 {
		t.Fatalf("explicit not coalesced: %s", log)
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSSHBoundedSettings(t *testing.T) {
	fakeTransportSSH(t)
	for name, value := range map[string]string{"MAX_ATTEMPTS": "1.5", "BACKOFF_INITIAL": "NaN", "BACKOFF_MULTIPLIER": "0", "BACKOFF_MAX": "Inf", "WAKE_GRACE": "121", "SHUTDOWN_TIMEOUT": "0", "REFRESH_DEBOUNCE": "6", "REFRESH_MAX_WAIT": "0"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("REMARKABLE_SSH_"+name, value)
			if _, err := NewSSH(); err == nil {
				t.Fatal("invalid setting accepted")
			}
		})
	}
}

func TestSSHCloseCancelsActiveAndQueued(t *testing.T) {
	b, dir := fakeTransportSSH(t)
	transportFixture(t, dir, "block", nil)
	active := make(chan error, 1)
	go func() { _, err := b.Mkdir(context.Background(), "active", ""); active <- err }()
	awaitSSH(t, func() bool { _, err := os.Stat(filepath.Join(dir, "started")); return err == nil })
	queued := make(chan error, 1)
	go func() { _, err := b.Mkdir(context.Background(), "queued", ""); queued <- err }()
	awaitSSH(t, func() bool { return b.Diagnostics()["queue_depth"] == 1 })
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-queued; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued close: %v", err)
	}
	var unknown *SSHExecutionUnknownError
	if err := <-active; !errors.As(err, &unknown) {
		t.Fatalf("active close: %v", err)
	}
	if !b.RefreshPending() {
		t.Fatal("close cleared uncertain pending state")
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if string(calls) != "call\n" {
		t.Fatalf("queued/replayed command: %s", calls)
	}
}

func TestSSHCloseDrainsOwedRefresh(t *testing.T) {
	b, dir := fakeTransportSSH(t)
	t.Setenv("REMARKABLE_DEFER_RESTART", "")
	result := make(chan error, 1)
	go func() { _, err := b.Mkdir(context.Background(), "write", ""); result <- err }()
	awaitSSH(t, func() bool { d := b.Diagnostics(); return d["refresh_pending"] == true && d["active"] == false })
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if b.RefreshPending() {
		t.Fatal("owed refresh not drained")
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "systemctl-calls"))
	if strings.Count(string(calls), "restart\n") != 1 {
		t.Fatalf("refresh: %s", calls)
	}
}

func TestSSHRefreshCancellationDrainsAndPreservesLaterWrite(t *testing.T) {
	b, dir := fakeTransportSSH(t)
	if _, err := b.Mkdir(context.Background(), "before", ""); err != nil {
		t.Fatal(err)
	}
	transportFixture(t, dir, "block-restart", nil)
	// Always release the fixture's remote shell, even if an assertion fails.
	t.Cleanup(func() { os.Remove(filepath.Join(dir, "block-restart")) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	refresh := make(chan error, 1)
	go func() { refresh <- b.RefreshTablet(ctx) }()
	awaitSSH(t, func() bool { _, err := os.Stat(filepath.Join(dir, "restarting")); return err == nil })
	cancel()
	select {
	case err := <-refresh:
		t.Fatalf("cancellation returned before drain: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	later := make(chan error, 1)
	go func() { _, err := b.Mkdir(context.Background(), "after", ""); later <- err }()
	awaitSSH(t, func() bool { return b.Diagnostics()["queue_depth"] == 1 })
	if err := os.Remove(filepath.Join(dir, "block-restart")); err != nil {
		t.Fatal(err)
	}
	if err := <-refresh; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-later; err != nil {
		t.Fatal(err)
	}
	if !b.RefreshPending() {
		t.Fatal("refresh consumed a later deferred mutation")
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSSHCloseDeadlineLeavesRefreshDraining(t *testing.T) {
	b, dir := fakeTransportSSH(t)
	if _, err := b.Mkdir(context.Background(), "pending", ""); err != nil {
		t.Fatal(err)
	}
	transportFixture(t, dir, "block-restart", nil)
	t.Cleanup(func() { os.Remove(filepath.Join(dir, "block-restart")) })
	refresh := make(chan error, 1)
	go func() { refresh <- b.RefreshTablet(context.Background()) }()
	awaitSSH(t, func() bool { _, err := os.Stat(filepath.Join(dir, "restarting")); return err == nil })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := b.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unbounded close: %v", err)
	}
	if !b.RefreshPending() {
		t.Fatal("deadline cleared pending refresh")
	}
	if err := os.Remove(filepath.Join(dir, "block-restart")); err != nil {
		t.Fatal(err)
	}
	if err := <-refresh; err != nil {
		t.Fatal(err)
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSSHCancelBackoffDoesNotDirtyOrReplay(t *testing.T) {
	_, dir := fakeTransportSSH(t)
	t.Setenv("REMARKABLE_SSH_BACKOFF_INITIAL", "1")
	t.Setenv("REMARKABLE_SSH_BACKOFF_MAX", "1")
	t.Setenv("FAKE_SSH_FAILURE", "refused")
	b, err := NewSSH()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := b.Mkdir(ctx, "cancel", ""); result <- err }()
	awaitSSH(t, func() bool { return b.Diagnostics()["retries"] == uint64(1) })
	cancel()
	var pre *SSHPreExecutionError
	if err := <-result; !errors.As(err, &pre) || !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if b.RefreshPending() {
		t.Fatal("pre-execution cancellation marked dirty")
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if string(calls) != "call\n" {
		t.Fatalf("replayed after cancel: %s", calls)
	}
}
