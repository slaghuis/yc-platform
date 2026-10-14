# Integration Tests

Cross-service integration tests that live outside any one service.

Prefer per-service integration tests in `services/<name>/tests/integration/`.
Use this directory only when the test genuinely spans multiple services and
isn't an E2E (which goes in `test/e2e/`).