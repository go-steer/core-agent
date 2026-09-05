# Example overlay — gke-platform-agent

Copy this directory, edit the values below for your environment, and
`kubectl apply -k` it. This overlay uses the **OCI image volume** content
delivery (base default); for clusters below GKE 1.35 use
[`../initcontainer-copy`](../initcontainer-copy/) instead.

Most people should not edit this by hand. `scripts/set-up-demo.sh`
rewrites every value listed below from `scripts/prereqs.sh`, applies the
result, and then asserts that no placeholder survived into the rendered
manifest — see [`../../../DEMO.md`](../../../DEMO.md). Edit directly only
if you are deploying without the operator rig.

## What to edit

1. **`images:`** — pin the three images: the daemon
   (`ghcr.io/go-steer/core-agent`), the watcher
   (`ghcr.io/go-steer/lookout`), and the content image
   (`gke-platform-agent-content`), which is one **you build and push**
   from [`../../content.Dockerfile`](../../content.Dockerfile). The
   `configurations: kustomizeconfig/images.yaml` line is what lets the
   `images:` block reach the image-*volume* reference — kustomize's
   default field list does not cover it.
2. **`configMapGenerator` (`core-agent-gcp-env`)** — `GOOGLE_CLOUD_PROJECT`
   and `GOOGLE_CLOUD_LOCATION` (the Vertex endpoint, normally `global`),
   plus `GKE_CLUSTER` and `GKE_LOCATION` (the cluster this deployment
   triages). The two `*_LOCATION` values are different things and folding
   them fails asymmetrically: the daemon boots, `gke` reads work, and only
   the model call 404s.
3. **`patch-watcher-args.yaml`** — `--cluster-name` must name the same
   cluster as `GKE_CLUSTER`, and `--owner` must be a key in the bearer
   table. That file explains both.

## Before applying

Create the two Secrets out-of-band (see
[`../../base/20-secrets-placeholder.md`](../../base/20-secrets-placeholder.md))
and bind the daemon KSA's Workload Identity roles (see
[`../../base/10-serviceaccount-daemon.yaml`](../../base/10-serviceaccount-daemon.yaml)).

## Adding remote-cluster watchers

The watcher Deployment is a template. To cover another cluster, deploy a
second watcher into it with `--cluster-name=<that-cluster>` and
`--daemon-url=https://<this-daemon-external-endpoint>:7777`, plus a copy
of the `lookout-watch-token` Secret. The hub daemon stays single.
