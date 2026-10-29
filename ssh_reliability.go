package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var ErrSSHClosed = errors.New("SSH dispatcher is shutting down")

type sshTicket struct {
	ready   chan struct{}
	cancel  context.CancelFunc
	cleanup bool
}
type sshRefresh struct {
	done chan struct{}
	err  error
}
type sshReliability struct {
	mu                                  sync.Mutex
	queue                               []*sshTicket
	closing                             bool
	changed                             chan struct{}
	refresh                             *sshRefresh
	known                               bool
	pendingSnapshot                     atomic.Bool
	epoch                               uint64 // protected by SSHBackend.mu
	attempts                            int
	initial, multiplier, maximum, grace float64
	debounce, maxWait, shutdown         time.Duration
	readyTimeout, poll, settle          time.Duration
	retries                             uint64
	refreshes                           uint64
	lastError                           string
}

func (r *sshReliability) configure() error {
	settings := []struct {
		name          string
		def, min, max float64
		dst           *float64
	}{
		{"BACKOFF_INITIAL", .5, 0, 30, &r.initial}, {"BACKOFF_MULTIPLIER", 2, 1, 10, &r.multiplier},
		{"BACKOFF_MAX", 2, 0, 60, &r.maximum}, {"WAKE_GRACE", 8, 0, 120, &r.grace},
	}
	read := func(name string, def, min, max float64) (float64, error) {
		v := def
		if s, ok := os.LookupEnv("REMARKABLE_SSH_" + name); ok {
			var err error
			v, err = strconv.ParseFloat(s, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid REMARKABLE_SSH_%s", name)
			}
		}
		if math.IsNaN(v) || math.IsInf(v, 0) || v < min || v > max {
			return 0, fmt.Errorf("REMARKABLE_SSH_%s must be between %g and %g", name, min, max)
		}
		return v, nil
	}
	for _, s := range settings {
		v, e := read(s.name, s.def, s.min, s.max)
		if e != nil {
			return e
		}
		*s.dst = v
	}
	r.attempts = 4
	if raw, ok := os.LookupEnv("REMARKABLE_SSH_MAX_ATTEMPTS"); ok {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > 10 {
			return errors.New("REMARKABLE_SSH_MAX_ATTEMPTS must be an integer between 1 and 10")
		}
		r.attempts = v
	}
	for _, s := range []struct {
		name          string
		def, min, max float64
		dst           *time.Duration
	}{
		{"REFRESH_DEBOUNCE", .15, 0, 5, &r.debounce}, {"REFRESH_MAX_WAIT", 1, .01, 30, &r.maxWait}, {"SHUTDOWN_TIMEOUT", 5, .1, 60, &r.shutdown},
	} {
		v, e := read(s.name, s.def, s.min, s.max)
		if e != nil {
			return e
		}
		*s.dst = time.Duration(v * float64(time.Second))
	}
	return r.configureReadiness()
}

func (b *SSHBackend) run(ctx context.Context, command string, input []byte, timeout time.Duration) ([]byte, error) {
	r := &b.reliability
	started := time.Now()
	for attempt := 1; ; attempt++ {
		out, err := b.runOnce(ctx, command, input, timeout)
		var pre *SSHPreExecutionError
		if !errors.As(err, &pre) || ctx.Err() != nil {
			return out, err
		}
		delay := time.Duration(math.Min(r.initial*math.Pow(r.multiplier, float64(attempt-1)), r.maximum) * float64(time.Second))
		if attempt >= r.attempts || time.Since(started)+delay > time.Duration(r.grace*float64(time.Second)) {
			return nil, fmt.Errorf("wake and unlock the tablet; connection failed after %d attempt(s): %w", attempt, err)
		}
		r.mu.Lock()
		r.retries++
		r.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, &SSHPreExecutionError{ctx.Err()}
		case <-timer.C:
		}
	}
}

// acquire hands ownership directly to the next ticket, preserving FIFO even
// under contention. Cancelled waiters remove themselves without taking a turn.
func (b *SSHBackend) acquire(ctx context.Context, cleanup bool) (context.Context, func(), error) {
	r := &b.reliability
	ctx, cancel := context.WithCancel(ctx)
	t := &sshTicket{ready: make(chan struct{}), cancel: cancel, cleanup: cleanup}
	r.mu.Lock()
	if r.closing && !cleanup {
		r.mu.Unlock()
		cancel()
		return nil, nil, ErrSSHClosed
	}
	if ctx.Err() != nil {
		r.mu.Unlock()
		cancel()
		return nil, nil, ctx.Err()
	}
	r.queue = append(r.queue, t)
	if len(r.queue) == 1 {
		close(t.ready)
	}
	r.mu.Unlock()
	release := func() {
		cancel()
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, q := range r.queue {
			if q == t {
				r.queue = append(r.queue[:i], r.queue[i+1:]...)
				if i == 0 && len(r.queue) > 0 {
					close(r.queue[0].ready)
				}
				break
			}
		}
		if r.changed != nil {
			close(r.changed)
			r.changed = nil
		}
	}
	select {
	case <-ctx.Done():
		release()
		return nil, nil, ctx.Err()
	case <-t.ready:
	}
	if ctx.Err() != nil {
		release()
		return nil, nil, ctx.Err()
	}
	return ctx, release, nil
}

func sshCall[T any](b *SSHBackend, ctx context.Context, write bool, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	job, release, err := b.acquire(ctx, false)
	if err != nil {
		return zero, err
	}
	b.mu.Lock()
	before := b.reliability.epoch
	b.mu.Unlock()
	value, err := fn(job)
	b.mu.Lock()
	dirty := b.reliability.epoch != before
	b.mu.Unlock()
	deferred, _ := ctx.Value(deferredRestartContextKey{}).(bool)
	var refresh *sshRefresh
	if write && dirty && !deferred && !Enabled("REMARKABLE_DEFER_RESTART") {
		refresh = b.requestRefresh(true)
	}
	release()
	if refresh != nil {
		// Drain safety refresh even when the caller cancels an active mutation.
		<-refresh.done
		if ctx.Err() != nil {
			return value, errors.Join(err, ctx.Err())
		}
		if refresh.err != nil {
			return value, fmt.Errorf("write may have persisted; tablet refresh pending: %w", errors.Join(err, refresh.err))
		}
	}
	return value, err
}

func (b *SSHBackend) markDirty() {
	b.pending = true
	b.reliability.pendingSnapshot.Store(true)
	b.reliability.epoch++
}

// Called with the operation mutex held. Failed multi-step shell commands can
// persist changes, so only a proven pre-execution failure is clean.
func (b *SSHBackend) runChange(ctx context.Context, command string, input []byte, timeout time.Duration) error {
	_, err := b.run(ctx, command, input, timeout)
	var pre *SSHPreExecutionError
	if !errors.As(err, &pre) {
		b.markDirty()
	}
	return err
}

func (b *SSHBackend) requestRefresh(allowClosing bool) *sshRefresh {
	r := &b.reliability
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing && !allowClosing {
		f := &sshRefresh{done: make(chan struct{}), err: ErrSSHClosed}
		close(f.done)
		return f
	}
	if r.refresh != nil {
		return r.refresh
	}
	if r.known && !r.pendingSnapshot.Load() {
		f := &sshRefresh{done: make(chan struct{})}
		close(f.done)
		return f
	}
	f := &sshRefresh{done: make(chan struct{})}
	r.refresh = f
	go func() {
		delay := r.debounce
		if delay > r.maxWait {
			delay = r.maxWait
		}
		time.Sleep(delay)
		ctx, cancel := context.WithTimeout(context.Background(), r.readyTimeout+60*time.Second)
		defer cancel()
		job, release, err := b.acquire(ctx, true)
		if err == nil {
			r.mu.Lock()
			known := r.known
			r.mu.Unlock()
			b.mu.Lock()
			clean := known && !b.pending
			b.mu.Unlock()
			if !clean {
				err = b.sshRefreshTablet(job)
			}
		}
		if err != nil {
			b.mu.Lock()
			b.pending = true
			r.pendingSnapshot.Store(true)
			b.mu.Unlock()
		}
		r.mu.Lock()
		if err == nil {
			r.known = true
			r.refreshes++
			r.lastError = ""
		} else {
			r.lastError = err.Error()
		}
		f.err = err
		r.refresh = nil
		close(f.done)
		if r.changed != nil {
			close(r.changed)
			r.changed = nil
		}
		r.mu.Unlock()
		if release != nil {
			release()
		}
	}()
	return f
}

func (b *SSHBackend) RefreshTablet(ctx context.Context) error {
	if Enabled("REMARKABLE_READ_ONLY") {
		return ErrTransportReadOnly
	}
	r := &b.reliability
	r.mu.Lock()
	closing := r.closing
	r.mu.Unlock()
	if closing {
		return ErrSSHClosed
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f := b.requestRefresh(false)
	select {
	case <-ctx.Done():
		// Caller cancellation must not interrupt a restart already owed by writes.
		<-f.done
		return errors.Join(ctx.Err(), f.err)
	case <-f.done:
		return f.err
	}
}

// Close rejects new work, cancels queued/active ordinary operations, and drains
// any safety refresh already owed. A deadline failure never clears pending.
func (b *SSHBackend) Close(ctx context.Context) error {
	r := &b.reliability
	ctx, cancel := context.WithTimeout(ctx, r.shutdown)
	defer cancel()
	r.mu.Lock()
	r.closing = true
	for _, t := range r.queue {
		if !t.cleanup {
			t.cancel()
		}
	}
	for len(r.queue) > 0 || r.refresh != nil {
		if r.changed == nil {
			r.changed = make(chan struct{})
		}
		changed := r.changed
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
		r.mu.Lock()
	}
	r.mu.Unlock()
	return nil
}

func (b *SSHBackend) Diagnostics() map[string]any {
	pending := b.reliability.pendingSnapshot.Load()
	r := &b.reliability
	r.mu.Lock()
	defer r.mu.Unlock()
	depth := len(r.queue)
	active := depth > 0
	if active {
		depth--
	}
	return map[string]any{"queue_depth": depth, "active": active, "closing": r.closing, "refresh_pending": pending, "pending_state_known": r.known, "refreshes": r.refreshes, "retries": r.retries, "last_error": r.lastError}
}

func (b *SSHBackend) List(ctx context.Context) ([]Document, error) {
	return sshCall(b, ctx, false, b.sshList)
}
func (b *SSHBackend) Download(ctx context.Context, id string) (map[string][]byte, error) {
	return sshCall(b, ctx, false, func(c context.Context) (map[string][]byte, error) { return b.sshDownload(c, id) })
}
func (b *SSHBackend) Upload(ctx context.Context, data []byte, ext, parent, name string) (Document, error) {
	return sshCall(b, ctx, true, func(c context.Context) (Document, error) { return b.sshUpload(c, data, ext, parent, name) })
}
func (b *SSHBackend) Mkdir(ctx context.Context, name, parent string) (Document, error) {
	return sshCall(b, ctx, true, func(c context.Context) (Document, error) { return b.sshMkdir(c, name, parent) })
}
func sshMutation(b *SSHBackend, ctx context.Context, fn func(context.Context) error) error {
	_, err := sshCall(b, ctx, true, func(c context.Context) (struct{}, error) { return struct{}{}, fn(c) })
	return err
}
func (b *SSHBackend) Rename(ctx context.Context, id, name string) error {
	return sshMutation(b, ctx, func(c context.Context) error { return b.sshRename(c, id, name) })
}
func (b *SSHBackend) Delete(ctx context.Context, id string) error {
	return sshMutation(b, ctx, func(c context.Context) error { return b.sshDelete(c, id) })
}
func (b *SSHBackend) DeletePermanently(ctx context.Context, id string) error {
	return sshMutation(b, ctx, func(c context.Context) error { return b.sshDeletePermanently(c, id) })
}
func (b *SSHBackend) Move(ctx context.Context, id, parent string) error {
	return sshMutation(b, ctx, func(c context.Context) error { return b.sshMove(c, id, parent) })
}
func (b *SSHBackend) Overwrite(ctx context.Context, id string, pdf []byte) error {
	return sshMutation(b, ctx, func(c context.Context) error { return b.sshOverwrite(c, id, pdf) })
}
func (b *SSHBackend) PutDocumentFiles(ctx context.Context, id string, files map[string][]byte) error {
	return sshMutation(b, ctx, func(c context.Context) error { return b.sshPutDocumentFiles(c, id, files) })
}
