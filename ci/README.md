# Proposed CI

`ci.yaml` is a GitHub Actions workflow for the operator to move to `.github/workflows/`.

| Job | Runs |
|---|---|
| `test` | `make verify`, `make test`, `make dist` |
| `e2e` | `make e2e` on kind |
