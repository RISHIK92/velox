package judge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/rishik92/velox/engine"
)

// P5-W30-S05: the queue and the pool against a real Valkey (the local one, or
// VALKEY_URL), with a fake sandbox. Skipped without a Valkey.
func store(t *testing.T) Store {
	t.Helper()
	url := os.Getenv("VALKEY_URL")
	if url == "" {
		url = "redis://localhost:6379"
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(opts)
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("no Valkey at %s: %v", url, err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	s := Store{Client: client, Prefix: "judge-test-" + hex.EncodeToString(b)}
	t.Cleanup(func() {
		keys, _ := client.Keys(context.Background(), s.Prefix+":*").Result()
		if len(keys) > 0 {
			client.Del(context.Background(), keys...)
		}
		client.Close()
	})
	return s
}

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[:8], h[8:12], h[12:16], h[16:20], h[20:])
}

func job(id, priority, stdin string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"id": id, "priority": priority, "language": "python",
		"files":       []map[string]string{{"name": "main.py", "content": "print(1)"}},
		"limits":      map[string]int{"seconds": 2, "memoryMB": 64},
		"runs":        []map[string]any{{"stdin": stdin, "returnOutput": true}},
		"submittedAt": "2026-11-12T10:00:00Z",
	})
	return raw
}

// fake records which jobs ran, in order, and can hold them until released.
type fake struct {
	mu    sync.Mutex
	order []string
	hold  chan struct{}
}

func (f *fake) Run(ctx context.Context, j engine.Job) (engine.Result, error) {
	f.mu.Lock()
	f.order = append(f.order, j.Stdin)
	f.mu.Unlock()
	if f.hold != nil {
		<-f.hold
	}
	return engine.Result{Verdict: engine.OK, Stdout: "out:" + j.Stdin}, nil
}

func (f *fake) ran() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

func worker(s Store, r Runner, n int) *Worker {
	return &Worker{Store: s, Runner: r, Concurrency: n, Lease: 2 * time.Second, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func result(t *testing.T, s Store, id string) JudgeResult {
	t.Helper()
	vals, err := s.Client.BLPop(context.Background(), 20*time.Second, s.reply(id)).Result()
	if err != nil {
		t.Fatalf("no result for %s: %v", id, err)
	}
	r, err := DecodeJudgeResult([]byte(vals[1]))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func serve(t *testing.T, w *Worker) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Serve(ctx) }()
	return func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}

func TestHighestPriorityFirst(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	ids := map[string]string{}
	for _, p := range []string{"run", "casual", "rooms", "ranked"} {
		ids[p] = uuid()
		if err := s.Submit(ctx, p, job(ids[p], p, p)); err != nil {
			t.Fatal(err)
		}
	}
	f := &fake{}
	stop := serve(t, worker(s, f, 1))
	for _, p := range Priorities {
		result(t, s, ids[p])
	}
	stop()
	got := f.ran()
	if fmt.Sprint(got) != fmt.Sprint(Priorities) {
		t.Fatalf("ran %v, want %v", got, Priorities)
	}
}

func TestAJobRunsOnceAndItsResultIsStored(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	id := uuid()
	for range 3 {
		if err := s.Submit(ctx, "casual", job(id, "casual", "x")); err != nil {
			t.Fatal(err)
		}
	}
	f := &fake{}
	stop := serve(t, worker(s, f, 2))
	r := result(t, s, id)
	time.Sleep(3 * time.Second)
	stop()
	if n := len(f.ran()); n != 1 {
		t.Fatalf("ran %d times", n)
	}
	if *r.Status != "done" || *r.Runs[0].Stdout != "out:x" {
		t.Fatalf("%+v", r)
	}
	if n, _ := s.Client.Exists(ctx, s.result(id)).Result(); n != 1 {
		t.Error("no stored result")
	}
}

func TestAJobWhoseWorkerDiedIsTakenBack(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	id := uuid()
	// A worker took it, leased it, and died.
	orphan(t, s, id, "rooms", job(id, "rooms", "orphan"), time.Now().Add(-time.Second))
	f := &fake{}
	stop := serve(t, worker(s, f, 1))
	r := result(t, s, id)
	stop()
	if *r.Status != "done" {
		t.Fatalf("%+v", r)
	}
	if left, _ := s.Client.HLen(ctx, s.running()).Result(); left != 0 {
		t.Errorf("%d leases left", left)
	}
}

func TestAMalformedJobFailsWithoutRunning(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	id := uuid()
	bad, _ := json.Marshal(map[string]any{"id": id, "priority": "casual", "language": "cobol"})
	if err := s.Submit(ctx, "casual", bad); err != nil {
		t.Fatal(err)
	}
	// And one with no id at all, and one that isn't JSON: dropped, logged.
	for _, raw := range []string{`{"nope":1}`, `not json`} {
		if err := s.Submit(ctx, "casual", []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	f := &fake{}
	stop := serve(t, worker(s, f, 1))
	r := result(t, s, id)
	stop()
	if *r.Status != "failed" || r.Error == nil || len(f.ran()) != 0 {
		t.Fatalf("%+v ran %v", r, f.ran())
	}
}

func TestStoppingFinishesTheJobsTaken(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	id := uuid()
	if err := s.Submit(ctx, "ranked", job(id, "ranked", "slow")); err != nil {
		t.Fatal(err)
	}
	f := &fake{hold: make(chan struct{})}
	stop := serve(t, worker(s, f, 1))
	for len(f.ran()) == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	time.Sleep(300 * time.Millisecond)
	select {
	case <-stopped:
		t.Fatal("stopped before its job finished")
	default:
	}
	close(f.hold)
	<-stopped
	if *result(t, s, id).Status != "done" {
		t.Fatal("the job taken wasn't finished")
	}
}

func TestManyJobsAcrossWorkers(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	ids := []string{}
	for i := range 20 {
		id := uuid()
		ids = append(ids, id)
		if err := s.Submit(ctx, Priorities[i%4], job(id, Priorities[i%4], fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	f := &fake{}
	stop := serve(t, worker(s, f, 4))
	for _, id := range ids {
		result(t, s, id)
	}
	stop()
	if len(f.ran()) != 20 {
		t.Fatalf("ran %d", len(f.ran()))
	}
}

func orphan(t *testing.T, s Store, id, priority string, raw []byte, until time.Time) {
	t.Helper()
	lease, _ := json.Marshal(map[string]any{"owner": "dead-worker", "until": until.Unix(), "priority": priority, "job": string(raw)})
	if err := s.Client.HSet(context.Background(), s.running(), id, lease).Err(); err != nil {
		t.Fatal(err)
	}
}

func TestADuplicateIsDroppedWhileItsJobRunsAndAfter(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	id := uuid()
	raw := job(id, "casual", "once")
	if err := s.Submit(ctx, "casual", raw); err != nil {
		t.Fatal(err)
	}
	f := &fake{hold: make(chan struct{})}
	stop := serve(t, worker(s, f, 2))
	for len(f.ran()) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	// While it runs: claimed by no one.
	if err := s.Submit(ctx, "casual", raw); err != nil {
		t.Fatal(err)
	}
	for n, _ := s.Client.LLen(ctx, s.queue("casual")).Result(); n > 0; n, _ = s.Client.LLen(ctx, s.queue("casual")).Result() {
		time.Sleep(20 * time.Millisecond)
	}
	close(f.hold)
	result(t, s, id)
	// After it's answered: dropped too.
	if err := s.Submit(ctx, "casual", raw); err != nil {
		t.Fatal(err)
	}
	for n, _ := s.Client.LLen(ctx, s.queue("casual")).Result(); n > 0; n, _ = s.Client.LLen(ctx, s.queue("casual")).Result() {
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	if n := len(f.ran()); n != 1 {
		t.Fatalf("ran %d times", n)
	}
}

func TestOnlyALeasesOwnerReleasesIt(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	id := uuid()
	orphan(t, s, id, "rooms", job(id, "rooms", "x"), time.Now().Add(time.Hour))
	if ok, err := s.Release(ctx, id, "someone-else"); err != nil || ok {
		t.Fatalf("released by a stranger: %v %v", ok, err)
	}
	if ok, err := s.Extend(ctx, id, "someone-else", time.Now()); err != nil || ok {
		t.Fatalf("extended by a stranger: %v %v", ok, err)
	}
	if ok, err := s.Release(ctx, id, "dead-worker"); err != nil || !ok {
		t.Fatalf("not released by its owner: %v %v", ok, err)
	}
}

func TestAnUnreadableLeaseIsRemovedAndTheRestReaped(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	if err := s.Client.HSet(ctx, s.running(), "garbage", "{not json").Err(); err != nil {
		t.Fatal(err)
	}
	id := uuid()
	orphan(t, s, id, "ranked", job(id, "ranked", "x"), time.Now().Add(-time.Second))
	back, broken, err := s.Reap(ctx, time.Now())
	if err != nil || back != 1 || broken != 1 {
		t.Fatalf("back %d broken %d err %v", back, broken, err)
	}
	if n, _ := s.Client.LLen(ctx, s.queue("ranked")).Result(); n != 1 {
		t.Fatalf("not requeued: %d", n)
	}
}

func TestAnOversizedJobFailsWithoutRunning(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	id := uuid()
	big, _ := json.Marshal(map[string]any{"id": id, "priority": "run", "pad": strings.Repeat("x", maxJobBytes)})
	if err := s.Submit(ctx, "run", big); err != nil {
		t.Fatal(err)
	}
	f := &fake{}
	stop := serve(t, worker(s, f, 1))
	r := result(t, s, id)
	stop()
	if *r.Status != "failed" || len(f.ran()) != 0 || len(*r.Error) > maxError {
		t.Fatalf("%+v", r)
	}
}
