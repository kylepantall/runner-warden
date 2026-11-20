# runner-warden

Autoscaling manager for self-hosted GitHub Actions runners on your own machines.

- One label per OS (`<prefix>-linux`, `<prefix>-windows`, `<prefix>-macos`), one fresh runner per job.
- Admits jobs by real memory use, so small jobs never wait behind big ones.
- Shared caches, hung-job reaping, orphan cleanup, one-command `runner-warden setup`.

**Status:** pre-alpha. Design in [PLAN.md](PLAN.md); spike findings in [spike/](spike/).

> ⚠️ Runners use the host Docker daemon. That is root-equivalent: only serve repos whose code you trust.

Licensed under [Apache-2.0](LICENSE).
