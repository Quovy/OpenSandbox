# OpenSandbox Claude Guide

See `AGENTS.md` for all rules, routing, and conventions.

## Quovy fork: durable learnings

- Sync the fork onto an upstream tag by `git cherry-pick -x` of the fork-only commits, then `git merge -s ours origin/main` into the branch: a replayed branch conflicts with `main` on every adapted or dropped hunk, and GitHub fires no `pull_request` CI while the PR is `DIRTY`. Merge the PR with `--merge`, never squash. The reviewable fork delta is `git diff <upstream-tag> main`.
- `components/egress` must stay on CPython 3.10/3.11 and the exact pins in `requirements-mitmproxy.txt`: `credential_bound_h2.py` is a build gate that rejects newer mitmproxy/h2/hyperframe. `curl` must stay in the egress image; `egress-test.yml` and `real-e2e.yml` `docker exec` it.
- The egress image force-installs `h11==0.16.0` and `Brotli==1.2.1` past mitmproxy 11.0.2's declared bounds (`--no-deps`): `publish-quovy-images.yml` rejects Critical Trivy findings and both pinned versions carry one. Re-check that step whenever the mitmproxy pin moves. Before a fork sync lands, `docker build` every publish-matrix image locally and `trivy image --severity CRITICAL` it; the matrix contexts are `kubernetes`, `server`, and `.` (see the workflow).
- After an upstream sync, drop the fork's `go.mod`/`go.sum`/`uv.lock` bumps: upstream refreshes them itself and they only conflict. Keep the Dockerfile digest pins and `apk` version floors.
- `components/execd/Dockerfile` has an `ebpf-builder` stage besides `builder`; pin both to the same `golang` digest. The image-committer Go test is `image_test.go` (was `main_test.go`).
