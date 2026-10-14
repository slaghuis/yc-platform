# Daily Workflow

```bash
# Start the dev stack if not already running
task devstack:up

# Run a service locally
task run SERVICE=yc-identity

# Run tests
task test
task test:integration

# Format and lint before committing
task fmt
task lint
```

Pre-commit hooks run automatically on `git commit`.
