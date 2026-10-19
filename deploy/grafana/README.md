# Rookery hub Grafana dashboard

`rookery-hub.json` shows message states, node contact, API requests, latency,
database size, snapshot age and the hub container's CPU and memory. It expects
the hub's metrics scraped as job `rookery-hub` (the ServiceMonitor in
`deploy/kubernetes/monitoring` does this). Grafana 10 or later.

## Import

1. Dashboards, New, Import, then upload `rookery-hub.json`.
2. Choose the Prometheus datasource in the `Prometheus` variable at the top.
3. Adjust `Scrape job` and `Namespace` if yours differ. CPU and memory come from
   the kubelet's cAdvisor metrics (`container_cpu_usage_seconds_total`,
   `container_memory_working_set_bytes`), not from the hub.

## Communication graph (optional)

The collapsed row "Communication graph (requires Infinity plugin)" draws a node
graph of which sessions message which, plus message activity per time bucket,
from the hub's `/graph` endpoint on the metrics port, for example
`http://rookery-hub.rookery.svc:8788/graph?from=<ms>&to=<ms>`.

- Install the
  [Infinity datasource plugin](https://grafana.com/grafana/plugins/yesoreyeram-infinity-datasource/)
  (`yesoreyeram-infinity-datasource`) and create a datasource from it. Add the
  hub's metrics URL to its allowed hosts.
- Select it in the `Infinity (graph row)` variable and set `Hub metrics URL`.
- The queries run server-side (backend parser), so Grafana itself, not the
  browser, must reach port 8788. Allow Grafana's namespace in the hub's
  NetworkPolicy if it is not the monitoring namespace.

`/graph` is unauthenticated and reveals session names and participants. Keep it
cluster-internal; never put a public route in front of it to feed Grafana.

Without the plugin, leave the row collapsed; the other panels do not depend on
it.

## Provisioning with the sidecar (optional)

With the kube-prometheus-stack or Grafana Helm chart sidecar, ship the file as
a ConfigMap carrying the label the sidecar watches (commonly
`grafana_dashboard: "1"`):

```sh
kubectl -n monitoring create configmap rookery-hub-dashboard \
  --from-file=rookery-hub.json=deploy/grafana/rookery-hub.json
kubectl -n monitoring label configmap rookery-hub-dashboard grafana_dashboard=1
```

The dashboard uses datasource variables, not import inputs, so it works
unchanged through the sidecar; pick the datasources once in the UI.
