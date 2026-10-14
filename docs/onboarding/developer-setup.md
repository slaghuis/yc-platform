# Developer Setup

How to prepare a workstation to work on yc-platform.

## Required Tools

| Tool | Minimum | Install |
|---|---|---|
| Go | 1.26 | https://go.dev/doc/install |
| Docker | 24+ | https://docs.docker.com/get-docker/ |
| Docker Compose | V2 | Included with Docker Desktop |
| Task | 3.30+ | `brew install go-task` or https://taskfile.dev/installation/ |
| Git | 2.30+ | OS package manager |

## Recommended Tools

| Tool | Install |
|---|---|
| golangci-lint | `brew install golangci-lint` or https://golangci-lint.run/usage/install/ |
| gofumpt | `go install mvdan.cc/gofumpt@latest` |
| goimports | `go install golang.org/x/tools/cmd/goimports@latest` |
| pre-commit | `brew install pre-commit` or `pipx install pre-commit` |
| migrate | `go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest` |
| jq | OS package manager |
| nats CLI | `brew install nats-io/nats-tools/nats` |

Verify installation:

```bash
task check-deps
```