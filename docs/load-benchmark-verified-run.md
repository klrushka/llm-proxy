# Verified 1000 RPS / 5 minute load run

This document records one verified canonical run of `cmd/pii-load` against the
`POST /process` benchmark adapter. It is a local-loopback, fast-mode result and
is **not** an official-checker result, a deployed-environment result, or a full
NER-worker throughput claim.

## Command

```sh
go run ./cmd/pii-load -base-url http://127.0.0.1:18082 -rps 1000 -duration 5m -pool 1000 -concurrency 200 -timeout 10s
```

The service ran separately in fast mode on local loopback. The command exited
`0`.

## Raw JSON report

```json
{
  "requested_rps": 1000,
  "achieved_rps": 1000.001750143063,
  "duration": 299999474958,
  "planned": 300000,
  "scheduled": 300000,
  "completed": 300000,
  "errors": 0,
  "unschedulable": 0,
  "p50": 356250,
  "p95": 727333,
  "p99": 1256375,
  "target_latency": 1000000000,
  "target_met": true,
  "target_is_appendix": true
}
```

## Reading the numbers

Go's `time.Duration` marshals to JSON as an integer number of nanoseconds.
Human-readable latency values:

- p50 = `356250` ns = **0.356250 ms**
- p95 = `727333` ns = **0.727333 ms**
- p99 = `1256375` ns = **1.256375 ms**
- target_latency = `1000000000` ns = **1 s**

The 1 second latency target is an Appendix product target (a goal), not a hard
rule of the official checker; the report marks it with `target_is_appendix:
true`. In this run `target_met` is `true` because p99 (1.256375 ms) is below the
1 s target.

## Evidence boundary

- Local loopback only; not a deployed environment.
- Fast mode (Go rules/validators only); not a full NER-worker throughput claim.
- Not the official checker result.
- Synthetic PII only; no real personal data was used.