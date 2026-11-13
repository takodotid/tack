# Measure how long a switchover and a failover keep writes down

Run this in staging, on real clusters, once for each mode: a planned switchover, and a failover after a site loss. Kind and CI show that the steps work. They do not show how long the steps take on the staging network, storage and DNS. The numbers measured here go into the failover-time budget.

Each measurement gives two numbers:

| Number | Where |
|---|---|
| What Tack recorded | `tack status RUN -f workload.yaml`, line `Writes down:`; `status.writes` in `-o yaml` or `-o json` |
| What a client saw | the longest time between two acknowledged writes, from the writer below |

The difference is the time outside Tack's window. Most of it is Strait's route change and the client's own reconnect.

## Before

| Check | How |
|---|---|
| A staging workload with the production layout | the same sites, Poolers, Strait services and WAL archives |
| `kubectl`, `jq` and `bash` on this host | `hack/measure/writes.sh` uses them |
| The address the applications use | the Strait service or DNS name the applications connect to: `HOST` |
| A Secret with the keys `username` and `password` of a user that may create a table | such as the app Secret that CloudNativePG makes, `<cluster>-app`: `SECRET`. The script never reads or prints it: the Pod reads it |

## Steps

1. **Start a writer** at a site that stays up, through the applications' address. It inserts one numbered row about every 0.2 s, and logs each attempt with its time:

   ```sh
   hack/measure/writes.sh start --context CTX --namespace NS --host HOST --secret SECRET
   ```

2. **Run Tack** as in production:
   - planned: `tack run -f workload.yaml --to NEW`
   - site loss: first cut the old site off, as in [site-loss.md](site-loss.md). Then `tack run -f workload.yaml --to NEW --mode site-loss`

3. **Stop the writer** a minute after the run ends, and keep its log:

   ```sh
   hack/measure/writes.sh stop --context CTX --namespace NS --out writes.log
   hack/measure/writes.sh report writes.log
   ```

4. **Check that no acknowledged write is lost.** After a planned switchover, the output must say `none`. After a site loss, writes that the old primary acknowledged after the fork point are expected to be lost. Compare them with `Fork:` in `tack status`.

   ```sh
   hack/measure/writes.sh lost writes.log --context NEW_CTX --namespace NS --host HOST --secret SECRET
   ```

5. **Record both numbers** with `tack status RUN -f workload.yaml`. Repeat each mode at least three times. Keep the largest of each number.

6. **Clean up:** run `drop table tack_measure;` at the new primary.

Put the largest numbers in the budget: [wal-rpo-rto.md](../wal-rpo-rto.md#the-failover-time-budget). What Tack records, and how long the demotion takes, are in [operations.md](../operations.md#how-long-writes-stay-down).
