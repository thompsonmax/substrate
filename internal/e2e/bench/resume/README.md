# Resume latency benchmark

Measures what `--inject-egress-trust-bundle` adds to actor activation. Two
layers, run on a Linux machine:

## Node-side microbenchmarks

The whole node-side cost is one `writeSystemInfoVolume` call per Run/Restore
(inside `prepareOCIBundles`, in parallel with the snapshot download), plus one
bind mount on gVisor. On micro-VM the system-info root is already one shared
directory and a FULL restore replays the guest mount table.

```shell
# resolve (informer-cache Get + PEM sanitize) and resolve+write, per anchor count
go test ./cmd/atelet -run '^$' -bench 'TrustBundle|SystemInfoVolume' -benchmem -count 5

# control-plane side: the spec mutation in ateapi
go test ./cmd/ateapi/internal/controlapi -run '^$' -bench InjectEgressTrust -benchmem -count 5
```

`TMPDIR` picks the filesystem the write lands on. Point it at the disk that
backs the node's actor directories to get representative rename costs.

## End-to-end on kind

`hack/bench-resume-injection-kind.sh` runs `TestResumeLatency` per sandbox
class with the flag off, flips the cluster the way CI's MITM step does
(sdsmint egress, ate-api-server with the flag, counter demos recreated so
their goldens carry the injected volume), and runs it again with the flag on.

```shell
# fresh machine: create the cluster, install, deploy demos, then both arms
hack/bench-resume-injection-kind.sh --setup

# existing cluster on the plain install
hack/bench-resume-injection-kind.sh

# more samples, one class
hack/bench-resume-injection-kind.sh --classes microvm --golden 10 --cycles 50
```

Per class and arm the test creates `BENCH_GOLDEN` actors and times the first
`ResumeActor` of each (a golden restore), then runs `BENCH_CYCLES`
pause/resume cycles on the first actor (local restores). `ResumeActor`
returns after atelet's Restore and the RUNNING commit, so the round trip is
the activation time. Afterwards it reads atelet's `Restore timing breakdown`
log lines for its actors, which carry the same phases as the
`ate.actor.restore.duration` histogram but per restore rather than bucketed.
The system-info write lands in `oci_unpack`; the extra mount, if any, in
`ateom_restore`.

Results accumulate under `__resume-bench/<timestamp>/`:

- `samples.csv`: one row per timed resume (`label,class,kind,actor,iter,ms`).
- `phases.csv`: one row per atelet phase per restore.
- `summary.txt`: p50/p90/max/mean per class, kind and label, arms on
  adjacent rows. Rewritten after every run over everything recorded so far.
- `arms.tsv`: the live ate-api-server args each arm was verified against.

Golden restores are few and noisy on micro-VM (a cloud-hypervisor restore);
the local rows are the ones to compare for the per-activation cost.
