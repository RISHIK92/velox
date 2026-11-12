package judge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// P5-W30-S07: POST /judge holds a job to its contract before it is queued; GET
// /judge/{id} answers 404 until a worker has finished it.
func TestJudgeOverHTTP(t *testing.T) {
	s := store(t)
	mux := http.NewServeMux()
	mux.Handle("POST /judge", s.SubmitHandler())
	mux.Handle("GET /judge/{id}", s.ResultHandler())
	call := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}

	for _, bad := range []string{`{}`, `not json`, `{"id":"x","priority":"casual","language":"cobol"}`} {
		if w := call("POST", "/judge", bad); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	if w := call("POST", "/judge", strings.Repeat(" ", maxJobBytes+1)); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized job: %d", w.Code)
	}

	id := uuid()
	w := call("POST", "/judge", string(job(id, "ranked", "")))
	if w.Code != http.StatusAccepted {
		t.Fatalf("a valid job: %d %s", w.Code, w.Body)
	}
	var ack struct{ ID string }
	if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil || ack.ID != id {
		t.Fatalf("ack %s: %v", w.Body, err)
	}
	ctx := context.Background()
	if n := s.Client.LLen(ctx, s.queue("ranked")).Val(); n != 1 {
		t.Fatalf("queued at its priority: %d", n)
	}

	if w := call("GET", "/judge/"+id, ""); w.Code != http.StatusNotFound {
		t.Fatalf("pending: %d", w.Code)
	}
	result := `{"id":"` + id + `","status":"done"}`
	if _, err := s.Finish(ctx, id, []byte(result)); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", "/judge/"+id, ""); w.Code != http.StatusOK || w.Body.String() != result {
		t.Fatalf("answered: %d %s", w.Code, w.Body)
	}
}
