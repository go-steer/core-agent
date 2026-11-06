# `kode-gopher` UAT against `kind`

Manual UAT harness for [`examples/kode-gopher`](../../../examples/kode-gopher) ([#1391](https://github.com/go-steer/core-agent/issues/1391)).

All UAT state lives under `/tmp/core-agent-uat-kode-gopher` — never `$HOME`.

## Commands

```bash
./dev/uat/kode-gopher/run.sh dry-run   # hermetic recipe test + echo one-shot check (no cluster / creds)
./dev/uat/kode-gopher/run.sh up        # create kind cluster + seed sample workloads
./dev/uat/kode-gopher/run.sh smoke     # run core-agent against kode-gopher on the kind cluster
./dev/uat/kode-gopher/run.sh down      # delete kind cluster and /tmp state
```
