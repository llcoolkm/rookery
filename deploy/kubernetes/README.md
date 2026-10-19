# Rookery hub on Kubernetes

Plain manifests for one hub in namespace `rookery`. They need no operator;
`monitoring/` is a separate, optional kustomization that needs the Prometheus
Operator CRDs, so the base applies on clusters without them.

| File | In the kustomization | Purpose |
| --- | --- | --- |
| `namespace.yaml`, `pvc.yaml`, `deployment.yaml`, `service.yaml` | yes | the hub |
| `networkpolicy.yaml` | yes | API only from the ingress controller, metrics only from monitoring |
| `secret.example.yaml` | no | token-hash Secret format |
| `ingress.example.yaml` | no | TLS Ingress for `POST /v2/rpc` (ingress-nginx) |
| `monitoring/` | separate | ServiceMonitor and PrometheusRule |

## Install

1. Create the token-hash Secret (below). The pod does not start without it.
   The kustomization never includes `secret.example.yaml`, so reapplying it
   cannot overwrite your real Secret with placeholders.
2. Review `deployment.yaml` (pin the image digest), `pvc.yaml` (storage class)
   and `networkpolicy.yaml`: its two namespace selectors, marked `ADAPT`,
   default to `ingress-nginx` (allowed to reach the API port) and `monitoring`
   (allowed to reach the metrics port). With other names the hub is
   unreachable from your Ingress or scraper until you change them.
3. Apply:

   ```sh
   kubectl apply -k deploy/kubernetes
   kubectl -n rookery rollout status deploy/rookery-hub
   ```

4. Copy `ingress.example.yaml`, set your host and TLS, and apply it.
5. Optionally `kubectl apply -k deploy/kubernetes/monitoring`.

## Tokens

Each node generates its own token and gives the hub only the SHA-256 hash; see
[Tokens](../../docs/operations.md#tokens) and the commands in
`secret.example.yaml`. Collect the hashes in a `tokens.json` such as
`{"alpha": "<64 hex>", "beta": "<64 hex>"}`, then:

```sh
kubectl create namespace rookery --dry-run=client -o yaml | kubectl apply -f -
kubectl -n rookery create secret generic rookery-hub-tokens \
  --from-file=tokens.json=./tokens.json
```

Keep `tokens.json` out of version control. The hub rereads the file on every
request and the kubelet refreshes Secret volumes, so rotation and revocation
need no restart (allow a minute or so for propagation).

## TLS

The hub speaks plain HTTP. Terminate TLS at the Ingress and route only
`POST /v2/rpc` on port 8787 (`ingress.example.yaml` routes the exact path; the
hub answers 404 to anything else on that port). Allow at least 60 seconds per
request for long-polls and bodies up to 256 KiB. Configure helpers with the
origin, for example `https://rookery.example.com`, without `/v2`.

## Storage

`pvc.yaml` asks for 1 GiB, ReadWriteOnce, from the default storage class. Use
local or block storage only. **Never NFS, SMB, CephFS or another network
filesystem**: SQLite's locking and fsync guarantees do not hold there and the
database can be corrupted. Budget for `--max-bytes` (100 MB by default), the WAL
and up to three snapshots, which the hub writes to `/data/state/backups`.

## Metrics are private

Port 8788 serves `/healthz`, `/readyz`, `/metrics` and `/graph` **without
authentication**. `/graph` returns session names and IDs, participants,
timestamps and counts. Never expose the metrics port publicly: no Ingress,
LoadBalancer or NodePort for it. The NetworkPolicy limits it to the monitoring
namespace; keep it that way, or tighter.

## Upgrade

1. Back up the database first. Copy the newest snapshot the hub took itself, or
   take a fresh one with SQLite's online backup API (never copy the live
   database file alone; committed state may still be in the WAL):

   ```sh
   umask 077
   dir=$(mktemp -d)   # new private directory, nothing to overwrite
   kubectl -n rookery exec deploy/rookery-hub -- python -c \
     'import os, sqlite3
   d = "/data/state/upgrade.sqlite3"
   assert not os.path.exists(d), d + " exists; remove it first"
   s = sqlite3.connect("/data/state/hub.sqlite3"); t = sqlite3.connect(d)
   s.backup(t); t.close(); s.close()'
   kubectl -n rookery exec deploy/rookery-hub -- cat /data/state/upgrade.sqlite3 > "$dir/hub.sqlite3"
   kubectl -n rookery exec deploy/rookery-hub -- rm /data/state/upgrade.sqlite3
   echo "backup: $dir/hub.sqlite3"
   ```

   The file contains message bodies and metadata; keep it private.

2. Read the release notes, then change the image tag and digest in
   `deployment.yaml` and apply again. `Recreate` stops the old pod before the
   new one starts, so expect a short gap; helpers keep their outboxes and retry.

### Restoring

Follow [Restoring](../../docs/operations.md#restoring) in the operations guide
exactly. In short: stop every helper on every node first; scale the hub to 0
and keep the old database with its `-wal` and `-shm` files; restore the backup
into a clean data directory (database owned by 10001, mode 0600, no stale
`-wal`, `-shm` or `.lock` files); scale to 1 and reconcile against the actual
agent histories; only then start the helpers again. A helper that runs against
the restored hub too early can deliver or resend work twice.

## One replica

The hub is one process with one SQLite database. Keep `replicas: 1` and strategy
`Recreate`. The hub takes an exclusive, non-blocking lock next to the
database, so a second pod sharing the volume would fail to start; RWO storage
alone does not fence processes on one node. There is no replication or
failover: Kubernetes gives restarts and rescheduling here, not high
availability.

## Monitoring

`monitoring/` holds a ServiceMonitor (job `rookery-hub`) and alerts: hub down,
silent always-on node (edit the `CHANGE-ME` node regex), a stuck dispatch,
uncertain messages, a large database and a stale snapshot. If your Prometheus
selects rules and monitors by label (kube-prometheus-stack uses
`release: <name>`), add that label. A Grafana dashboard is in
[`deploy/grafana`](../grafana/README.md).
