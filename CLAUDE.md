# OpenSandbox Claude Guide

See `AGENTS.md` for all rules, routing, and conventions.

## Quovy fork: durable learnings

- Sync the fork onto an upstream tag by `git cherry-pick -x` of the fork-only commits; merge the resulting PR with `--merge`, never squash, so `git log <upstream-tag>..main` stays the reviewable fork delta.
- `components/egress` must stay on CPython 3.10/3.11 and the exact pins in `requirements-mitmproxy.txt`: `credential_bound_h2.py` is a build gate that rejects newer mitmproxy/h2/hyperframe. `curl` must stay in the egress image; `egress-test.yml` and `real-e2e.yml` `docker exec` it.
- After an upstream sync, drop the fork's `go.mod`/`go.sum`/`uv.lock` bumps: upstream refreshes them itself and they only conflict. Keep the Dockerfile digest pins and `apk` version floors.
- `components/execd/Dockerfile` has an `ebpf-builder` stage besides `builder`; pin both to the same `golang` digest. The image-committer Go test is `image_test.go` (was `main_test.go`).
