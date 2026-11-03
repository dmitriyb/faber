# Data flow: Live view flow

```
tick (1 Hz) | key
   │
   ├─ store.AuditRuns ──► headers, replays ──► runs table (state via run lock, wall, live step)
   │
   ├─ store.Load(run) ──► Replay ──┬─► RunReporter{Targets}.Report ──► ReadingOrder ──► rows (tree/graph/timeline)
   │                               ├─► Starts − Results ──► running since
   │                               └─► output.log tail (pipeline.AttemptDir)
   │
   ├─ medians (once per view) ──► "usually", "slow"
   │
   └─ frame ──► one terminal write (+ bell on a new FAIL/HALT/pause)

key r / R ──► detached `faber run|resume … --progress json` ──► journal ──► next tick
key p     ──► store.RequestPause (live only) ──► the scheduler's pause gate ──► run-end paused
key q     ──► terminal restored ──► selected re-enter / resume command on stdout
```

Nothing flows back from the view into the engine except through the
commands and the pause marker the CLI already offers.
