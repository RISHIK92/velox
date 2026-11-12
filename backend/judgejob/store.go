package judge

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// P5-W30-S05 (LOAD-3, LOAD-5, ADR-0014): the judge's queue in Valkey. Jobs wait on one
// list per priority. A worker claims the first job of the highest non-empty list and
// leases it in one atomic step (a Lua script), so a job is never out of the queue
// without a lease: a worker that dies at any point leaves it for the reaper. A lease
// names its owner; only the owner extends or releases it. A job already leased or
// already answered is a duplicate and is dropped where it's claimed (LOAD-5).

// Priorities, highest first (LOAD-3: ranked, then rooms, then casual, then "Run tests").
// Strict, as LOAD-3 reads: under sustained ranked load, "run" waits (an owner question).
var Priorities = []string{"ranked", "rooms", "casual", "run"}

// How long a result and its reply are kept: long enough for any submitter to collect
// it, and the window in which a resubmitted id is recognised as done.
const resultTTL = time.Hour

type Store struct {
	Client *redis.Client
	// Keys start with it: "judge" in production; tests use their own.
	Prefix string
}

func (s Store) queue(priority string) string { return s.Prefix + ":queue:" + priority }
func (s Store) running() string              { return s.Prefix + ":running" }
func (s Store) result(id string) string      { return s.Prefix + ":result:" + id }
func (s Store) reply(id string) string       { return s.Prefix + ":reply:" + id }

// Submit queues a job (for Go callers and tests; TS has its own client).
func (s Store) Submit(ctx context.Context, priority string, job []byte) error {
	return s.Client.RPush(ctx, s.queue(priority), job).Err()
}

// claim pops queued jobs, highest priority first, until one can be leased: a job that
// is unreadable is returned unleased (the worker answers it as failed, or drops it); a
// duplicate of one leased or answered is discarded. Each lease is
// {owner, until, priority, job} under the job's id.
var claim = redis.NewScript(`
local prefix, owner, deadline = ARGV[1], ARGV[2], tonumber(ARGV[3])
for i = 4, #ARGV do
  local list = prefix .. ":queue:" .. ARGV[i]
  while true do
    local raw = redis.call("LPOP", list)
    if not raw then break end
    local ok, job = pcall(cjson.decode, raw)
    if not ok or type(job) ~= "table" or type(job.id) ~= "string" or #job.id > 64 then
      return {"bad", raw, ARGV[i]}
    end
    local id = job.id
    if redis.call("EXISTS", prefix .. ":result:" .. id) == 0
      and redis.call("HEXISTS", prefix .. ":running", id) == 0 then
      redis.call("HSET", prefix .. ":running", id,
        cjson.encode({owner = owner, ["until"] = deadline, priority = ARGV[i], job = raw}))
      return {"job", raw, ARGV[i]}
    end
  end
end
return false
`)

// Claim takes the next job for owner, leased until the given time. kind is "job", or
// "bad" for an unreadable one (not leased); empty when the queues are empty.
func (s Store) Claim(ctx context.Context, owner string, until time.Time) (kind string, raw []byte, priority string, err error) {
	args := append([]any{s.Prefix, owner, until.Unix()}, toAny(Priorities)...)
	res, err := claim.Run(ctx, s.Client, nil, args...).Slice()
	if errors.Is(err, redis.Nil) {
		return "", nil, "", nil
	}
	if err != nil {
		return "", nil, "", err
	}
	return res[0].(string), []byte(res[1].(string)), res[2].(string), nil
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// Only the lease's owner may change it: a worker whose lease ran out (and whose job was
// taken back) can't release or extend the new owner's.
var extend = redis.NewScript(`
local raw = redis.call("HGET", KEYS[1], ARGV[1])
if not raw then return 0 end
local lease = cjson.decode(raw)
if lease.owner ~= ARGV[2] then return 0 end
if ARGV[3] == "release" then
  redis.call("HDEL", KEYS[1], ARGV[1])
else
  lease["until"] = tonumber(ARGV[3])
  redis.call("HSET", KEYS[1], ARGV[1], cjson.encode(lease))
end
return 1
`)

// Extend moves owner's lease on a job to the given time; false when it isn't owner's.
func (s Store) Extend(ctx context.Context, id, owner string, until time.Time) (bool, error) {
	n, err := extend.Run(ctx, s.Client, []string{s.running()}, id, owner, until.Unix()).Int()
	return n == 1, err
}

// Release ends owner's lease on a job; false when it isn't owner's.
func (s Store) Release(ctx context.Context, id, owner string) (bool, error) {
	n, err := extend.Run(ctx, s.Client, []string{s.running()}, id, owner, "release").Int()
	return n == 1, err
}

// Finish stores a job's result once and hands it to its waiting submitter. false when
// another worker finished it first (its result stands).
func (s Store) Finish(ctx context.Context, id string, result []byte) (bool, error) {
	first, err := s.Client.SetNX(ctx, s.result(id), result, resultTTL).Result()
	if err != nil || !first {
		return first, err
	}
	pipe := s.Client.TxPipeline()
	pipe.RPush(ctx, s.reply(id), result)
	pipe.Expire(ctx, s.reply(id), resultTTL)
	_, err = pipe.Exec(ctx)
	return true, err
}

// reap takes back, at the front of its list, every job whose lease ran out (its worker
// died or stalled), in one atomic step per job; an unreadable lease is removed and
// counted, never left to block the rest.
var reap = redis.NewScript(`
local now, back, broken = tonumber(ARGV[2]), 0, 0
local all = redis.call("HGETALL", KEYS[1])
for i = 1, #all, 2 do
  local ok, lease = pcall(cjson.decode, all[i + 1])
  if not ok or type(lease) ~= "table" or type(lease.job) ~= "string" then
    redis.call("HDEL", KEYS[1], all[i])
    broken = broken + 1
  elseif tonumber(lease["until"]) <= now then
    redis.call("HDEL", KEYS[1], all[i])
    redis.call("LPUSH", ARGV[1] .. ":queue:" .. lease.priority, lease.job)
    back = back + 1
  end
end
return {back, broken}
`)

// Reap re-queues jobs whose lease ran out; it reports how many, and how many leases it
// removed as unreadable.
func (s Store) Reap(ctx context.Context, now time.Time) (requeued, broken int, err error) {
	res, err := reap.Run(ctx, s.Client, []string{s.running()}, s.Prefix, now.Unix()).Int64Slice()
	if err != nil {
		return 0, 0, err
	}
	return int(res[0]), int(res[1]), nil
}
