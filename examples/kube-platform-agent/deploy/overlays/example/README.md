# Example overlay — kube-platform-agent

Copy this directory, edit the values below for your environment, and
`kubectl apply -k` it. This overlay uses the **OCI image volume** content
delivery (base default); for clusters below GKE 1.35 use
[`../initcontainer-copy`](../initcontainer-copy/) instead.

## What to edit

1. **`images:`** — pin the three images. The content image
   (`kube-platform-agent-content`) is one **you build and push** from
   `deploy/content.Dockerfile` (see the recipe README's "Deploy" section);
   set its digest here. The `configurations: kustomizeconfig/images.yaml`
   line is what lets the `images:` block reach the image-*volume*
   reference — kustomize's default field list doesn't cover it.
2. **`configMapGenerator` (`core-agent-gcp-env`)** — your GCP project +
   Vertex location.
3. **`patch-watcher-cluster-name.yaml`** — set `--cluster-name` to your
   real cluster name so inject payloads identify their source.

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
