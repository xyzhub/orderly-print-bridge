# Workflow — orderly-print-bridge

Satellite of `xyzhub/orderly`; follows that repo's Agentic Workflow. Only the rows that differ are recorded here.

## §10 Project profile

| Row | Value |
|---|---|
| **Default branch** | `main` |
| **Test gate** | `go vet ./...` · `gofmt -l .` · `go test ./... -race` (CI: `ci.yml` on every PR) |
| **Deploy + live-verify** | none — a box pulls a release when it self-updates; the live check is a box enrolling and printing |
| **Merge policy** | agent-may-merge — owner, 2026-09-08 12:36: "you tag it, managing the orderly-box-image and orderly-print-bridge is yours." Green CI + a Fable review; log the merge in the `orderly` mission ledger it serves. |
| **Release** | `v*` tags are cut by the agent under the same delegation; `release.yml` builds the binaries and creates the GitHub Release |
