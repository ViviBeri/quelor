# Benchmarks

`go run ./cmd/quelor-bench` runs three deterministic scarcity scenarios and two
measurements against the in-process engine (`bench/scenarios.go`). It does not touch the
network, a real GitHub API or a real clock for the scenarios: they run on a fake clock
that the scenario advances.

## Run

```
go run ./cmd/quelor-bench                 # full report, text
go run ./cmd/quelor-bench -json           # same report as JSON
go run ./cmd/quelor-bench -units 2000     # smaller shared-bucket scenario
go test -run xxx -bench . -benchtime 20000x ./bench/   # Go micro-benchmarks
```

`-units` is the number of units each of three holders submits in the shared-bucket
scenario (default 20000). The seed is fixed at 1, so the scenario rows are reproducible;
the latency and throughput lines are wall-clock and vary run to run.

## Measurements

- **Acquire latency**: 20000 sequential `AcquireLease` calls on a one-resource engine
  with effectively unlimited slots, each settled after it is timed. Reports p50 and p99.
- **Throughput**: for 200 ms, `GOMAXPROCS` workers loop acquire then settle on the same
  engine. Reports completed pairs per second.

## Scenarios

| Scenario | Setup | What is counted as correct |
| --- | --- | --- |
| `shared-token-bucket` | 3 holders, each submitting `units` demands of 1, shuffled by the seed, against a `window_bucket` of 1000 per hour. After the window, 100 more units from one holder. | After the 1000th admit the bucket is empty; every later arrival must be `DEFER`. `into_empty` counts admits made into an empty bucket. The post-window batch must admit again (`resumed_admits`). |
| `429-storm` | A `window_bucket` configured at 400 per hour whose true limit is 100. Six windows: each window admits as many units as allowed, settles the first 100 as successes and the rest as HTTP 429 with an observation. Then 5 real-defect failures are classified. | The number of 429s in windows 2 to 6 must be below window 1 (AIMD shrank the burst), and every 429 must classify as `resource-exhausted` while the real defects classify as `real-defect`. The false-red rate is misclassified failures divided by all failures. |
| `dependency-flap` | A `circuit` resource, threshold 3, open for 30 s. The dependency is down for 120 s, then up for 60 s; one arrival per simulated second. | While the circuit is open every arrival must be `DEFER`. Notes report admits while the dependency was down against the bound (threshold plus down time divided by open time), and whether traffic resumed within one open period of recovery. |

The scenario oracles are written in `bench/scenarios.go`; read them before relying on a
"correctness" figure. The headline correctness number is the mean over the three
scenarios of `correct_defers / total_empty`. It measures whether this engine behaves as
the scenario author expects, not how Quelor compares with another system. No competitor
is benchmarked here.

## Results

One run of the default command (`go run ./cmd/quelor-bench`) on an Apple M1 Pro (10
cores), Go 1.27.3, darwin/arm64, which took about 72 seconds in total:

```
p50/p99 acquire: 11.916µs / 50.458µs
throughput: 58684.84 acquire+settle pairs/s
correctness under scarcity: 1.000000
false-red rate: 0.000000
scenario                  units admitted deferred     shed   denied   into_empty correct_defers total_empty
shared-token-bucket       60100     1100    59000        0        0            0          59000       59000
  notes: resumed_admits=100 seed=1
429-storm                  2400      405     1995        0        0            0              5           5
  notes: w429=300,0,0,0,0,0 wadmits=400,1,1,1,1,1 real_defect_ok=true misclassified=0 failed=305 seed=1
dependency-flap             180       64      116        0        0            0            116         116
  notes: admitted_while_down=6 bound=7 resumed=true resume_within_open_for=true open_arrivals=116 open_defers=116 seed=1
```

Reading the `429-storm` row: window 1 admitted 400 units and 300 drew a 429; in windows
2 to 6 the engine admitted one unit per window (`wadmits=400,1,1,1,1,1`) because the
halving of effective capacity drove it to the `min_capacity` floor of 1 within the first
window. The scenario therefore shows the multiplicative decrease and not a recovery
curve (the scenario comment says successes deliberately report no HTTP status so that
additive increase does not mask the decrease).

Go micro-benchmarks from the same machine, `-benchtime 20000x`:

```
BenchmarkAcquireSettle-10           20000     16078 ns/op    11729 B/op    114 allocs/op
BenchmarkAcquireParallel-10         20000     15882 ns/op    11725 B/op    114 allocs/op
BenchmarkAcquireDeferPath-10        20000    210595 ns/op   345981 B/op     87 allocs/op
BenchmarkMultiResourceAcquire-10    20000     19083 ns/op    13112 B/op    137 allocs/op
```

## Limits of these numbers

- They are in-process calls with the reference Decider and no trace writer. They exclude
  HTTP, JSON, a remote Decider, and disk.
- One machine, one run. Latency and throughput are not stable across runs; a second run
  of the same command on the same machine gave p50 10.9 µs and 62.5k pairs/s.
- `BenchmarkAcquireDeferPath` defers a distinct unit on every iteration against a
  one-slot resource that is already pinned, so each op takes the refusal branch and the
  engine records state per deferred unit. It is about 13 times the admit path here; treat
  it as a worst-case refusal cost, not steady state.
- CI runs the benchmarks with `-run xxx -bench . -benchtime 100x` as a smoke test only.
