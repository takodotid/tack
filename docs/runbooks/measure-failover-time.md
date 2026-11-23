# Measure how long a switchover and a failover keep writes down

Run this in staging, on real clusters, once per mode: a planned switchover and a failover after a site loss. Kind and CI numbers do not show step times on the staging network, storage and DNS. The numbers go into [the failover-time budget](../wal-rpo-rto.md#the-failover-time-budget).

Each measurement gives two numbers; their difference is the time outside Tack's window.

| Number | Where |
|---|---|
| What Tack recorded | `tack status RUN -f workload.yaml`, line `Writes down:`; `status.writes` in `-o yaml` or `-o json` ([How long writes stay down](../operations.md#how-long-writes-stay-down)) |
| What a client saw | the longest time between two acknowledged writes, from the writer below |

## Before

| Check | How |
|---|---|
| A staging workload with the production layout | the same sites, Poolers, Strait services and WAL archives |
| `kubectl`, `jq` and `bash` on this host | `hack/measure/writes.sh` uses them |
| The address the applications use | the Strait service or DNS name they connect to: `HOST` |
| A Secret with the keys `username` and `password` of a user that may create a table | such as the app Secret that CloudNativePG makes, `<cluster>-app`: `SECRET`. The Pod reads it; the script never reads or prints it. |

## Steps

1. Start a writer at a site that stays up, through the applications' address. It inserts one numbered row about every 0.2 s, and logs each attempt with its time:

   ```sh
   hack/measure/writes.sh start --context CTX --namespace NS --host HOST --secret SECRET
   ```

2. Run Tack as in production:
   - planned: `tack run -f workload.yaml --to NEW`
   - site loss: first cut the old site off, as in [site-loss.md](site-loss.md). Then `tack run -f workload.yaml --to NEW --mode site-loss`

3. Stop the writer a minute after the run ends, and keep its log:

   ```sh
   hack/measure/writes.sh stop --context CTX --namespace NS --out writes.log
   hack/measure/writes.sh report writes.log
   ```

4. Check that no acknowledged write is lost. After a planned switchover, the output must say `none`. After a site loss, writes the old primary acknowledged after the fork point are expected to be lost: compare them with `Fork:` in `tack status`.

   ```sh
   hack/measure/writes.sh lost writes.log --context NEW_CTX --namespace NS --host HOST --secret SECRET
   ```

5. Record both numbers. Repeat each mode at least three times and keep the largest of each number in the budget.

6. Clean up: run `drop table tack_measure;` at the new primary.
