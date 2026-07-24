# Data-Plane Intelligence Layer — moved to `go-steer/k8s-lookout`

This work now lives in [`go-steer/k8s-lookout`](https://github.com/go-steer/k8s-lookout):

- **Current spec:** [`docs/DESIGN.md`](https://github.com/go-steer/k8s-lookout/blob/main/docs/DESIGN.md)
  (v3.x — consolidated multicall binary, topology index, signal-engine sentinel,
  health scorecard, cloud-provider boundary).
- **This file's original v2.0 content** (the 25-binary matrix): preserved there as
  [`docs/appendix-v2-dataplane-intelligence.md`](https://github.com/go-steer/k8s-lookout/blob/main/docs/appendix-v2-dataplane-intelligence.md),
  historical and non-normative.

`cmd/k8s-event-watcher` moves to k8s-lookout in its M0 milestone (becoming
`lookout watch`, behavior-identical); it remains here until that lands, after
which `core-agent` drops its `k8s.io/*` dependencies entirely — per the
"daemon stays k8s-agnostic" policy in `docs/k8s-event-agent-design.md` and
`DESIGN.md`.
