# Platform settings

Defaults used by the platform runbooks when a request does not carry its
own parameters.

```
project_id:        acme-platform-prod
cluster:           eval-fixture-orders
location:          us-east4
primary_namespace: retail-prod
primary_workload:  storefront-web
```

`primary_workload` is the estate's flagship service. It is the subject
the weekly audit reports on.
