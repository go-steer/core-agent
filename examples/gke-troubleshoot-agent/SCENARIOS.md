# Additional k8s-event-watcher scenarios

Trigger-and-revert recipes for Event reasons other than `ImagePullBackOff` (which `DEMO.md` already covers). All assume:

- kube context points at the demo cluster
- target workloads live in `online-boutique`
- k8s-event-watcher runs with its default reason allow-list (`CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`, `OOMKilled`, `FailedMount`, `FailedScheduling`, `BackOff`, `Unhealthy`, `NetworkNotReady`, `NodeNotReady`, `Evicted`)

Each scenario is a single `kubectl` command to trigger and a single `rollout undo` (or equivalent) to revert.

---

## OOM on startup — productcatalogservice

- **Reason**: `OOMKilled` → `CrashLoopBackOff`
- **Concept**: Set memory limit below the catalog's boot-time footprint. Container is killed before it can serve.
- **Trigger**:
  ```bash
  kubectl -n online-boutique set resources deployment/productcatalogservice \
      --limits=memory=16Mi --requests=memory=16Mi
  ```
- **Expected**: New replica terminated with `Reason: OOMKilled` in `kubectl describe pod`. `Last State: Terminated` shows the memory limit. Frontend catalog calls begin failing within ~30s.
- **Revert**: `kubectl -n online-boutique rollout undo deployment/productcatalogservice`

---

## Missing upstream — checkoutservice (log-required)

- **Reason**: `CrashLoopBackOff`
- **Concept**: checkoutservice dials several upstream services on startup. Break one env var and it crashes; `describe` shows only `exit code 1`. The container log is the only place the failing upstream is named.
- **Trigger**:
  ```bash
  kubectl -n online-boutique set env deployment/checkoutservice \
      SHIPPING_SERVICE_ADDR=shippingservice:9999
  ```
- **Expected**: `CrashLoopBackOff` within ~60s. `kubectl logs deployment/checkoutservice --previous` shows `could not connect to shippingservice: dial tcp shippingservice:9999: ...`. `describe` reports exit 1 with no reason.
- **Revert**: `kubectl -n online-boutique rollout undo deployment/checkoutservice`

---

## Missing Secret — emailservice

- **Reason**: `FailedMount`
- **Concept**: Mount a Secret that doesn't exist. Fires immediately, no traffic needed.
- **Trigger**:
  ```bash
  kubectl -n online-boutique patch deployment emailservice --patch '
  spec:
    template:
      spec:
        volumes:
          - name: creds
            secret: { secretName: smtp-credentials-typo }
        containers:
          - name: server
            volumeMounts:
              - { name: creds, mountPath: /etc/creds, readOnly: true }'
  ```
- **Expected**: New replica stuck in `ContainerCreating`. Event with reason `FailedMount` names `smtp-credentials-typo`. Original replica remains Running.
- **Revert**: `kubectl -n online-boutique rollout undo deployment/emailservice`

---

## Impossible nodeSelector — adservice

- **Reason**: `FailedScheduling`
- **Concept**: Constrain the pod to a node label no node carries. Exercises the scheduler path (not kubelet).
- **Trigger**:
  ```bash
  kubectl -n online-boutique patch deployment adservice \
      -p '{"spec":{"template":{"spec":{"nodeSelector":{"zone":"does-not-exist"}}}}}'
  ```
- **Expected**: New replica in `Pending` within seconds. `FailedScheduling` event with `0/N nodes are available: ... didn't match Pod's node affinity/selector`.
- **Revert**: `kubectl -n online-boutique rollout undo deployment/adservice`

---

## Bad liveness path — frontend

- **Reason**: `Unhealthy` (watcher requires `count >= 3`)
- **Concept**: Point liveness probe at a nonexistent path. loadgenerator supplies enough traffic that the count threshold is reached in seconds; kubelet eventually restarts the pod.
- **Trigger**:
  ```bash
  kubectl -n online-boutique patch deployment frontend --type=json -p='[
    {"op":"replace","path":"/spec/template/spec/containers/0/livenessProbe/httpGet/path","value":"/does-not-exist"}
  ]'
  ```
- **Expected**: Repeated `Unhealthy` events on the pod (`Liveness probe failed: HTTP probe failed with statuscode: 404`). Watcher fires once count reaches 3. Pod eventually restarts and cycles.
- **Revert**: `kubectl -n online-boutique rollout undo deployment/frontend`

---

## Dependency down — cartservice ↔ redis-cart (log-required)

- **Reason**: `Unhealthy` (cartservice readiness)
- **Concept**: Scale Redis to zero. cartservice stays Running but its readiness probe (which pings Redis) fails. `describe` shows only `readiness probe failed: HTTP 503`; the log is where the Redis dial failure surfaces.
- **Trigger**:
  ```bash
  kubectl -n online-boutique scale deployment/redis-cart --replicas=0
  ```
- **Expected**: `Unhealthy` events on cartservice within ~15s. `kubectl logs deployment/cartservice` shows `redis: connection refused: redis-cart:6379` (or equivalent). cartservice pod stays Running throughout.
- **Revert**: `kubectl -n online-boutique scale deployment/redis-cart --replicas=1`

---

## Bad env value — paymentservice (log-required)

- **Reason**: `CrashLoopBackOff`
- **Concept**: Node.js app validates env at boot and throws on parse error. `describe` shows exit 1; only the log names the offending variable.
- **Trigger**:
  ```bash
  kubectl -n online-boutique set env deployment/paymentservice DISABLE_PROFILER=not-a-bool
  ```
- **Expected**: `CrashLoopBackOff` within ~30s. `logs --previous` shows a parse error naming `DISABLE_PROFILER` with the expected type.
- **Revert**: `kubectl -n online-boutique rollout undo deployment/paymentservice`

---

## Downstream 500 in readiness — frontend (log-required)

- **Reason**: `Unhealthy` (readiness returns 500)
- **Concept**: Frontend liveness is trivial, but readiness calls productcatalog. Break the upstream address → readiness returns 500 → `Unhealthy` fires. Pod itself looks healthy.
- **Trigger**:
  ```bash
  kubectl -n online-boutique set env deployment/frontend \
      PRODUCT_CATALOG_SERVICE_ADDR=productcatalogservice:9999
  ```
- **Expected**: `Unhealthy` events after count reaches 3. `describe` reports only `readiness probe failed: HTTP status code 500`. Frontend logs show the productcatalog dial failure on `:9999`. Pod stays Running.
- **Revert**: `kubectl -n online-boutique rollout undo deployment/frontend`
