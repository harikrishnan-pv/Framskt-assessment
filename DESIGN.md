# Snack Counter Automation — Design

**Problem.** Two offices — Oslo (~15 volunteers, 6,000 NOK/month, typical purchase 300–1,200 NOK) and Bengaluru (~20 volunteers, 15,000 INR/month, typical purchase 500–3,000 INR). Volunteers buy snacks out-of-pocket when the box empties; boxes sit empty for days (bystander effect), two people occasionally buy the same day, a few regulars always pay and wait 1–10 days for reimbursement, and management has zero spend visibility. Goal: one chat-first Go service, no new hardware.

## Clarifying questions & assumptions implemented

1. **Is the budget a hard stop?** Volunteers already spent personal money; blocking reimbursement punishes them. → **Soft cap:** flag the purchase `OverBudget`, DM the office manager, still submit to HR.
2. **Who converts currencies?** HR reimburses employees in their payroll currency. → Claims go to HR in the **office-local currency** (NOK/INR); we separately snapshot an FX-to-NOK rate at purchase time for reporting only.
3. **Presence source of truth?** Calendar/HR integration means brittle third-party permissions. → v1 uses chat status commands (`/snack away`, `/snack visiting oslo`); calendar sync is deferred.
4. **Reporting base currency?** Management asked for one monthly figure. → **NOK**, using purchase-time snapshot rates.

## Architecture

A single static Go binary — standard library only (~15 MB binary, ~12 MB idle RAM) on a minimal micro-VPS — is proportionate to ~35 users and a ~$2,000/month combined budget. The `snack` package holds pure business logic behind three ports: `ChatPort` (DMs and channel posts — chat is the only UI), `HRPort` (`amount`, `currency`, `description`, `receipt_image`, `employee_id`), and `FXPort` (rates into NOK). Every state transition runs under one mutex, which makes claim locking atomic; the clock is injected, so TTLs and ATTR are deterministic in tests. In-memory adapters simulate the three externals; production bindings are thin wrappers around chat webhooks and the HR REST API.

## Refill lifecycle: OPEN → CLAIMED → SUBMITTED → REIMBURSED

Three complementary triggers open a cycle:

1. **Reactive:** a laminated QR code taped inside the box deep-links to `/snack empty <office>`.
2. **Predictive (ATTR):** the Average Time To Refill is a rolling mean of the last 10 completed-refill intervals per office (default 5 days until history exists). When quiet time exceeds ATTR and no cycle is open, the bot proactively nudges.
3. **Freshness:** purchases carry a one-tap shelf-life preset (`3d`/`7d`/`30d+`). Perishable stock that passes its shelf life with no completed refill since raises an EXPIRY alert naming possibly slow-moving categories.

**Fair selection.** Eligible = present today (`IsPresent`) and active in that office (`ActiveOffice`), so WFH/holiday volunteers are never asked and visitors spending 1–2 weeks in the other office join the host rotation. Ranking: fewest refills in 90 days, tie broken by oldest last refill. The bot DMs one accountable person with `[I'll buy it]` / `[Can't today]` buttons instead of broadcasting to a channel.

**Atomic claim & timeout.** `ClaimRefill` flips OPEN → CLAIMED under the mutex; concurrent losers get `ErrAlreadyClaimed`. Claims expire after **24 h**; a sweeper reverts them to OPEN, records the stale claimer in the cycle's skip list, and DMs the next fairest volunteer — falling back to a channel broadcast when nobody is eligible. Declines hand off the same way, immediately.

## Receipts, budget guardrails & HR sync

Completing a refill logs amount, description, 1–3 category tags (Fruit, Nuts, Chocolate, Savory, Drinks), shelf life, and the receipt photo. Totals outside the typical range are sanity-flagged, never blocked — HR audits the attached image anyway. Over budget → manager alert + flag, claim still submitted. `SyncHRReimbursements` polls HR, flips SUBMITTED → REIMBURSED on approval, and DMs the volunteer.

## Reporting

`GenerateMonthlyReport` sums each office in local currency, converts with the snapshot rates into one NOK total, and ranks category popularity — answering "what are we spending" and "which snacks actually get eaten" in one artifact.

## Trade-offs: deliberately not built

- **No CCTV/computer vision or weight sensors.** Breakroom monitoring raises GDPR/Norwegian workplace-surveillance concerns, and hardware across two countries is disproportionate to the snack budget.
- **No OCR receipt parsing.** Volunteer totals + typical-range checks avoid fragile OCR; HR audits receipts regardless.
- **No web frontend or custom auth.** Everyone already lives in chat; a portal duplicates SSO and adds UI maintenance.
- **No calendar/HR presence integration in v1.** Chat toggles solve hybrid and visitor cases without brittle permissions.
- **No SKU/barcode inventory.** Tags + ATTR + expiry alerts answer popularity questions without making volunteers warehouse clerks.

**Tests.** `snack/service_test.go` proves: concurrent double-claim rejection, 24 h abandoned-claim handoff past WFH volunteers, cross-office visitor purchases, soft budget breach with HR submission intact, unified NOK reporting + tag analytics, ATTR/expiry triggers, and HR reimbursement sync.
