package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rishik92/velox/auth/middleware"
)

// Only operator-listed API keys reach /judge; with none listed, nobody does.
func TestJudgeKeyGate(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	as := func(gate func(http.Handler) http.Handler, key string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/judge", nil)
		r = r.WithContext(context.WithValue(r.Context(), middleware.APIKeyIDKey, key))
		gate(ok).ServeHTTP(w, r)
		return w.Code
	}
	gate := judgeKeyGate(" key-a , key-b,,")
	for key, want := range map[string]int{"key-a": 202, "key-b": 202, "key-c": 403, "": 403} {
		if got := as(gate, key); got != want {
			t.Errorf("%q: %d, want %d", key, got, want)
		}
	}
	if got := as(judgeKeyGate(""), "key-a"); got != http.StatusForbidden {
		t.Errorf("none listed: %d", got)
	}
}
