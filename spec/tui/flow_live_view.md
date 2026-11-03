# Data flow: Live view flow

```
tick (1 Hz) | key
   │
   ├─ store.AuditRuns ──► journals by stat ──► replay cache (replayed only when a journal grew)
   │                                              │
   │                                              ├─► runs table (state via run lock, wall, live step)
   │                                              └─► history p50/p90/p99 (recomputed when the cache version moved) ──► "usually", "slow"
   │
   ├─ config mtime ──► project reload (validate, desugar, preflight: key probe, hook and resolver stats; images re-probed on a slow cadence)
   │
   ├─ replay(run) ──┬─► RunReporter{Targets}.Report ──► ReadingOrder ──► rows (tree / flow / timeline)
   │                ├─► Replay.Running ──► running since
   │                └─► output.log tail (pipeline.AttemptDir)
   │
   └─ frame ──► one terminal write (+ bell on a new FAIL/HALT/pause)

key r / R ──► detached `faber run|resume … --progress json` ──► journal ──► next tick
key p     ──► store.RequestPause (live only) ──► the scheduler's pause gate ──► run-end paused
key q     ──► terminal restored ──► selected re-enter / resume command on stdout
Ctrl-C / Ctrl-D (bytes in raw mode), SIGTERM, SIGHUP ──► the same exit path
```

Nothing flows back from the view into the engine except through the
commands and the pause marker the CLI already offers. A store error is
shown on the status row; the last good table stays.
