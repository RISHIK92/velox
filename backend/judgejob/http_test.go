package judge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// POST /judge holds a job to its contract and queues it in canonical form; a job belongs
// to the key that submitted it; GET /judge/{id} answers 404 until a worker has finished.
func TestJudgeOverHTTP(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	mux := http.NewServeMux()
	key := func(r *http.Request) string { return r.Header.Get("X-Key") }
	mux.Handle("POST /judge", s.SubmitHandler(key))
	mux.Handle("GET /judge/{id}", s.ResultHandler(key))
	call := func(as, method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-Key", as)
		mux.ServeHTTP(w, r)
		return w
	}

	for _, bad := range []string{`{}`, `not json`, `{"id":"x","priority":"casual","language":"cobol"}`} {
		if w := call("a", "POST", "/judge", bad); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	if w := call("a", "POST", "/judge", strings.Repeat(" ", maxJobBytes+1)); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized job: %d", w.Code)
	}

	id := uuid()
	w := call("a", "POST", "/judge", string(job(id, "ranked", "")))
	if w.Code != http.StatusAccepted {
		t.Fatalf("a valid job: %d %s", w.Code, w.Body)
	}
	var ack struct{ ID string }
	if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil || ack.ID != id {
		t.Fatalf("ack %s: %v", w.Body, err)
	}
	// Its own key may resubmit it (a worker drops the duplicate); another may not take it.
	if w := call("a", "POST", "/judge", string(job(id, "ranked", ""))); w.Code != http.StatusAccepted {
		t.Errorf("resubmitted by its key: %d", w.Code)
	}
	if w := call("b", "POST", "/judge", string(job(id, "ranked", ""))); w.Code != http.StatusConflict {
		t.Errorf("taken by another key: %d", w.Code)
	}
	if n := s.Client.LLen(ctx, s.queue("ranked")).Val(); n != 1 {
		t.Fatalf("queued at its priority: %d", n)
	}

	if w := call("a", "GET", "/judge/"+id, ""); w.Code != http.StatusNotFound {
		t.Fatalf("pending: %d", w.Code)
	}
	result := `{"id":"` + id + `","status":"done"}`
	if _, err := s.Finish(ctx, id, []byte(result)); err != nil {
		t.Fatal(err)
	}
	if w := call("a", "GET", "/judge/"+id, ""); w.Code != http.StatusOK || w.Body.String() != result {
		t.Fatalf("answered: %d %s", w.Code, w.Body)
	}
	if w := call("b", "GET", "/judge/"+id, ""); w.Code != http.StatusNotFound {
		t.Errorf("read by another key: %d", w.Code)
	}
}

// Go reads JSON keys case-insensitively and the claim script doesn't: the queue gets
// the job re-encoded, so both read the id the API acknowledged.
func TestJudgeQueuesTheCanonicalJob(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	id, other := uuid(), uuid()
	raw := strings.Replace(string(job(id, "casual", "")), `"id":`, `"id":"`+other+`","ID":`, 1)
	w := httptest.NewRecorder()
	s.SubmitHandler(func(*http.Request) string { return "a" }).ServeHTTP(w, httptest.NewRequest("POST", "/judge", strings.NewReader(raw)))
	if w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var ack struct{ ID string }
	_ = json.Unmarshal(w.Body.Bytes(), &ack)
	queued, err := s.Client.LIndex(ctx, s.queue("casual"), 0).Result()
	if err != nil {
		t.Fatal(err)
	}
	var lower map[string]json.RawMessage
	if err := json.Unmarshal([]byte(queued), &lower); err != nil {
		t.Fatal(err)
	}
	if _, upper := lower["ID"]; upper || string(lower["id"]) != `"`+ack.ID+`"` {
		t.Fatalf("acknowledged %s, queued %s", ack.ID, queued)
	}
}

func TestJudgeRefusesAFullQueue(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	fill := make([]any, maxQueued)
	for i := range fill {
		fill[i] = "x"
	}
	if err := s.Client.RPush(ctx, s.queue("run"), fill...).Err(); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.SubmitHandler(func(*http.Request) string { return "a" }).ServeHTTP(w, httptest.NewRequest("POST", "/judge", strings.NewReader(string(job(uuid(), "run", "")))))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d", w.Code)
	}
}
