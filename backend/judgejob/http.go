package judge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
)

// Judging over HTTP, in Interval's JudgeJob/JudgeResult contract. A job is read whole and
// held to its contract (DecodeJudgeJob: no unknown fields, every rule). What is queued
// is the job re-encoded from what was decoded, never the client's bytes: Go matches JSON
// keys case-insensitively, the worker's claim script doesn't, and the two must read the
// same id. A job belongs to the API key that submitted it: no other key reads its
// result, or takes its id first.

// A queue holds at most this many jobs; past it, submissions wait for the workers (503)
// rather than fill Redis. A count, not bytes, and checked before the push, so concurrent
// submits may overshoot it slightly: with jobs of up to maxJobBytes, four queues can hold
// about 32 GB at worst. Only operator-listed keys submit (cmd/api), and Interval's jobs
// are a few KB each.
const maxQueued = 1_000

// SubmitHandler serves POST /judge for the key owner(r) names: 202 with the job's id; 400
// for a job outside the contract; 409 for an id another key holds; 413 for one too
// large; 503 when its queue is full. Resubmitting one's own id is accepted and not queued
// again.
func (s Store) SubmitHandler(owner func(*http.Request) string) http.Handler {
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
		canonical, err := json.Marshal(job)
		if err != nil {
			http.Error(w, "failed to encode the job", http.StatusInternalServerError)
			return
		}
		ctx := r.Context()
		if n, err := s.Client.LLen(ctx, s.queue(*job.Priority)).Result(); err != nil {
			http.Error(w, "failed to read the queue", http.StatusInternalServerError)
			return
		} else if n >= maxQueued {
			http.Error(w, "the queue is full; retry later", http.StatusServiceUnavailable)
			return
		}
		key := owner(r)
		claimed, err := s.claimID(r, *job.Id, key)
		if err != nil {
			http.Error(w, "failed to record the job", http.StatusInternalServerError)
			return
		}
		switch claimed {
		case taken:
			http.Error(w, "the id is taken", http.StatusConflict)
			return
		case fresh:
			if err := s.Submit(ctx, *job.Priority, canonical); err != nil {
				// Unclaimed again, so a retry queues it.
				s.Client.Del(context.WithoutCancel(ctx), s.owner(*job.Id))
				http.Error(w, "failed to queue the job", http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": *job.Id})
	})
}

type idClaim int

const (
	fresh idClaim = iota // the id is this key's now: queue the job
	held                 // this key submitted it before: it is queued or answered already
	taken                // another key holds it
)

// An owner outlives any queue wait; Finish then shortens it to its result's resultTTL.
const ownerTTL = 24 * time.Hour

// claimID makes an id the key's until its result expires.
func (s Store) claimID(r *http.Request, id, key string) (idClaim, error) {
	ctx := r.Context()
	// Twice at most: the owner can expire between SetNX and Get, and then the id is free.
	for range 2 {
		ok, err := s.Client.SetNX(ctx, s.owner(id), key, ownerTTL).Result()
		if err != nil {
			return 0, err
		}
		if ok {
			return fresh, nil
		}
		got, err := s.Client.Get(ctx, s.owner(id)).Result()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if got == key {
			return held, nil
		}
		return taken, nil
	}
	return taken, nil
}

// ResultHandler serves GET /judge/{id}: the JudgeResult once a worker has answered it; 404
// while it is pending, after resultTTL, or when another key submitted it.
func (s Store) ResultHandler(owner func(*http.Request) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")
		holder, err := s.Client.Get(ctx, s.owner(id)).Result()
		if errors.Is(err, redis.Nil) || (err == nil && holder != owner(r)) {
			http.Error(w, "no such job", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "failed to read the job", http.StatusInternalServerError)
			return
		}
		raw, err := s.Client.Get(ctx, s.result(id)).Bytes()
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
