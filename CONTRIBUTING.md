# Contributing

## Dev setup

| Need | Version |
|---|---|
| Go | see `go.mod` |
| Docker | 25+ (integration tests) |
| Linux with cgroup v2 + systemd | for end-to-end runs |

```bash
go test -race ./...
RW_DOCKER_TESTS=1 go test ./internal/backend/docker/... ./internal/dockerproxy/...
```

## Rules

- **No literal prefixes** in product code, docs examples or tests (use `my-ci`). CI enforces it.
- Log messages are constant strings; variable data goes in structured fields.
- Tests assert behaviour and structured values, not human-readable text.
- Ownership is by metadata, never by name: nothing may touch a container, volume, network or runner without our labels.
- Sign off commits (`git commit -s`, [DCO](https://developercompliance.org/)).
- One change per PR; keep diffs local to the change (no repo-wide reformatting).

## Project CI

CI runs on GitHub-hosted runners only. This project must never run its own CI on self-hosted runners: a public repo would execute fork PRs on a maintainer's machine.
