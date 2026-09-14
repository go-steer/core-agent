# on-call notes

The cluster is reachable with `kubectl`; the kubeconfig is already
pointed at it and no context switch is needed.

We run several namespaces and no one person knows all of them. When
something is reported broken without a location, start by finding out
what namespaces exist.

Read-only credentials. Anything that would change the cluster comes back
`Forbidden`, which is intentional — diagnose and report, do not remediate.
