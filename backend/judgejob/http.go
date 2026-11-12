package judge

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/redis/go-redis/v9"
)

// P5-W30-S07 (ADR-0041 in Interval): judging over HTTP, for any client. A job is read
// whole, held to its contract (DecodeJudgeJob: no unknown fields, every rule) and queued
// at its own priority; its result is kept for resultTTL. A duplicate id is accepted and
// then dropped where a worker claims it (LOAD-5), so a client may safely resubmit.

// SubmitHandler serves POST /judge: 202 with the job's id, 400 for a job outside the
// contract, 413 for one too large.
func (s Store) SubmitHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJobBytes))
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "job too large", http.StatusRequestEntityTooLarge)
			return
		}
		if err != nil {
			http.Error(w, "unreadable body", http.StatusBadRequest)
			return
		}
		job, err := DecodeJudgeJob(raw)
		if err != nil {
			http.Error(w, "not a JudgeJob: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.Submit(r.Context(), *job.Priority, raw); err != nil {
			http.Error(w, "failed to queue the job", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": *job.Id})
	})
}

// ResultHandler serves GET /judge/{id}: the JudgeResult once a worker has answered, 404
// while it is pending (or after resultTTL).
func (s Store) ResultHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := s.Client.Get(r.Context(), s.result(r.PathValue("id"))).Bytes()
		if errors.Is(err, redis.Nil) {
			http.Error(w, "pending", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "failed to read the result", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	})
}
