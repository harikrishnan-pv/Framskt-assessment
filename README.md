# Snack Counter Automation

Chat-first snack refill automation for two offices (Oslo, Bengaluru): fairness rotation, atomic claims with 24h handoff, soft budget guardrails, HR reimbursement sync, and unified multi-currency (NOK) reporting. Pure Go standard library — design rationale in [DESIGN.md](DESIGN.md).

## Quickstart

```bash
go test -v ./...        # run the full test suite
go test -race ./...     # claim locking verified under the race detector
go vet ./...            # static analysis
go test -v ./snack -run TestConcurrentDoubleClaimPrevention   # a single test
```

## Repository layout

| Path | Purpose |
| --- | --- |
| `DESIGN.md` | 1–2 page design document: assumptions, architecture, trade-offs |
| `go.mod` | Module `snack-automation` (Go 1.22+, zero external dependencies) |
| `snack/models.go` | Domain entities, state enums, shelf-life presets, domain errors |
| `snack/ports.go` | `ChatPort`, `HRPort`, `FXPort` interfaces for external systems |
| `snack/service.go` | Core logic: refill state machine, claim locking, ATTR, budgets, reporting |
| `snack/adapters.go` | In-memory stand-ins: `FakeChatAdapter`, `FakeHRAdapter`, `StaticFXAdapter` |
| `snack/service_test.go` | Test suite proving every business rule |

## 10-minute walkthrough

1. `models.go` — entities and the `OPEN → CLAIMED → SUBMITTED → REIMBURSED` machine.
2. `ports.go` — the three seams to the outside world.
3. `service.go` — triggers, fairness rotation, atomic claims, budget guardrails, monthly report.
4. `adapters.go` — fakes used by tests (chat recorder, HR simulator, static FX).
5. `service_test.go` — eight test groups mirroring the product rules.
