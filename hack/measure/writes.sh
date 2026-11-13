#!/usr/bin/env bash
# Measures how long a run keeps writes down, as a client sees it, and checks that no acknowledged write is lost.
# The operator runs it in staging, around a real tack run: docs/runbooks/measure-failover-time.md.
#
#   writes.sh start  --context CTX --namespace NS --host HOST --secret SECRET [--database DB] [--interval S] [--image IMAGE]
#   writes.sh stop   --context CTX --namespace NS --out FILE
#   writes.sh report FILE
#   writes.sh lost   FILE --context CTX --namespace NS --host HOST --secret SECRET [--database DB] [--image IMAGE]
#
# The writer is a Pod named tack-measure that inserts one numbered row a time into the table tack_measure, through HOST,
# the address the applications use. It reads the user and password from the keys username and password of the Secret
# SECRET, such as the app Secret that CloudNativePG makes; this script never reads or prints them.
set -euo pipefail

# The image of the e2e's PostgreSQL, by digest (test/e2e/env/lib.sh).
image=ghcr.io/cloudnative-pg/postgresql:18.6-system-trixie@sha256:44d17e8570dc25055376148a2918840def5bef0193969be3e79101afa25eafc2
pod=tack-measure
db=app interval=0.2 ctx= ns= host= secret= out=

die() { echo "writes.sh: $*" >&2; exit 2; }

parse() {
	while [ $# -gt 0 ]; do
		case $1 in
		--context) ctx=$2 ;;
		--namespace) ns=$2 ;;
		--host) host=$2 ;;
		--secret) secret=$2 ;;
		--database) db=$2 ;;
		--interval) interval=$2 ;;
		--image) image=$2 ;;
		--out) out=$2 ;;
		*) die "unknown argument $1" ;;
		esac
		shift 2
	done
	[ -n "$ctx" ] && [ -n "$ns" ] || die "--context and --namespace are required"
}

# run starts a Pod that runs script with psql's connection settings, the password from the Secret.
run() {
	local name=$1 script=$2
	[ -n "$host" ] && [ -n "$secret" ] || die "--host and --secret are required"
	kubectl --context "$ctx" -n "$ns" apply -f - <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $name
  labels: {app.kubernetes.io/name: tack-measure}
spec:
  restartPolicy: Never
  containers:
  - name: writer
    image: $image
    command: ["bash", "-c", $(printf '%s' "$script" | jq -Rs .)]
    env:
    - {name: PGHOST, value: "$host"}
    - {name: PGDATABASE, value: "$db"}
    - {name: PGCONNECT_TIMEOUT, value: "2"}
    - {name: INTERVAL, value: "$interval"}
    - {name: PGUSER, valueFrom: {secretKeyRef: {name: "$secret", key: username}}}
    - {name: PGPASSWORD, valueFrom: {secretKeyRef: {name: "$secret", key: password}}}
YAML
}

case ${1:-} in
start)
	shift
	parse "$@"
	run "$pod" '
until psql -qAtc "create table if not exists tack_measure (n bigint primary key, at timestamptz not null default clock_timestamp())"; do sleep 1; done
n=0
while true; do
	n=$((n+1))
	if psql -qAtc "insert into tack_measure (n) values ($n)" >/dev/null 2>&1; then r=ok; else r=fail; fi
	echo "$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ) $r $n"
	sleep "$INTERVAL"
done'
	kubectl --context "$ctx" -n "$ns" wait pod "$pod" --for=condition=Ready --timeout=2m
	echo "writer started: run tack now, then: $0 stop --context $ctx --namespace $ns --out FILE"
	;;
stop)
	shift
	parse "$@"
	[ -n "$out" ] || die "--out is required"
	kubectl --context "$ctx" -n "$ns" logs "$pod" >"$out"
	kubectl --context "$ctx" -n "$ns" delete pod "$pod" --wait=false
	echo "log in $out: $0 report $out"
	;;
report)
	file=${2:-}
	[ -f "$file" ] || die "report takes the log file of stop"
	# The longest time between two acknowledged writes, with the last write before it and the first after it.
	awk '
	function secs(t,   d, h) { split(substr(t, 12, 12), h, ":"); return h[1]*3600 + h[2]*60 + h[3] + (substr(t, 1, 10) != day ? 86400 : 0) }
	NR == 1 { day = substr($1, 1, 10) }
	$2 == "ok" { s = secs($1); if (seen && s - last > gap) { gap = s - last; from = lastt; to = $1 } last = s; lastt = $1; seen = 1; ok++ }
	$2 == "fail" { fail++ }
	END {
		printf "acknowledged writes: %d; failed attempts: %d\n", ok, fail
		if (gap > 0) printf "longest time without an acknowledged write: %.1f s, after %s, until %s\n", gap, from, to
	}' "$file"
	;;
lost)
	file=${2:-}
	[ -f "$file" ] || die "lost takes the log file of stop"
	shift 2
	parse "$@"
	run "$pod-lost" 'psql -qAtc "select n from tack_measure order by n"'
	kubectl --context "$ctx" -n "$ns" wait pod "$pod-lost" --for=jsonpath='{.status.phase}'=Succeeded --timeout=2m
	present=$(mktemp)
	kubectl --context "$ctx" -n "$ns" logs "$pod-lost" >"$present"
	kubectl --context "$ctx" -n "$ns" delete pod "$pod-lost" --wait=false
	# An acknowledged write that the new primary lacks is lost.
	missing=$(awk '$2 == "ok" { print $3 }' "$file" | sort -n | comm -23 - <(sort -n "$present"))
	rm -f "$present"
	if [ -n "$missing" ]; then
		echo "lost: $(echo "$missing" | wc -l) acknowledged writes are not at the new primary: $(echo "$missing" | tr '\n' ' ')"
		exit 1
	fi
	echo "lost: none; every acknowledged write is at the new primary"
	;;
*)
	sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'
	exit 2
	;;
esac
