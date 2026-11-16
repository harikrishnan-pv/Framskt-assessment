package snack

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// testBase anchors the controllable clock at a fixed Monday morning.
var testBase = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// Test helpers.
// ---------------------------------------------------------------------------

// testClock is a controllable time source for deterministic time travel.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }
func (c *testClock) SetTo(t time.Time)       { c.now = t }

// newTestService wires a Service with in-memory adapters, static FX rates
// (1 INR = 0.085 NOK) and a clock anchored at testBase.
func newTestService(t *testing.T) (*Service, *FakeChatAdapter, *FakeHRAdapter, *testClock) {
	t.Helper()
	chat := &FakeChatAdapter{}
	hr := NewFakeHRAdapter()
	fx := StaticFXAdapter{Rates: map[Currency]float64{CurrencyINR: 0.085}}
	svc, err := New(chat, hr, fx, testConfig())
	check(t, "New", err)
	clk := &testClock{now: testBase}
	svc.WithClock(clk.Now)
	return svc, chat, hr, clk
}

// testConfig mirrors the two real offices from the brief.
func testConfig() Config {
	return Config{
		Offices: map[Office]OfficeConfig{
			OfficeOslo: {
				Currency:      CurrencyNOK,
				MonthlyBudget: 6000,
				ManagerID:     "mgr-oslo",
				Channel:       "ch-oslo",
				TypicalMin:    300,
				TypicalMax:    1200,
			},
			OfficeBengaluru: {
				Currency:      CurrencyINR,
				MonthlyBudget: 15000,
				ManagerID:     "mgr-blg",
				Channel:       "ch-blg",
				TypicalMin:    500,
				TypicalMax:    3000,
			},
		},
	}
}

// vol builds a volunteer with the common boilerplate filled in.
func vol(id, name string, home Office, cur Currency, active Office, present bool, count90d int) Volunteer {
	return Volunteer{
		ID:              id,
		Name:            name,
		HomeOffice:      home,
		PayrollCurrency: cur,
		ActiveOffice:    active,
		IsPresent:       present,
		RefillCount90d:  count90d,
	}
}

func check(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", what, err)
	}
}

func register(t *testing.T, s *Service, vs ...Volunteer) {
	t.Helper()
	for _, v := range vs {
		check(t, "RegisterVolunteer("+v.ID+")", s.RegisterVolunteer(v))
	}
}

func openRefill(t *testing.T, s *Service, office Office, reporter string) *Refill {
	t.Helper()
	r, err := s.ReportEmpty(office, reporter)
	check(t, "ReportEmpty", err)
	return r
}

func claim(t *testing.T, s *Service, refillID, volunteerID string) {
	t.Helper()
	check(t, "ClaimRefill", s.ClaimRefill(refillID, volunteerID))
}

func submit(t *testing.T, s *Service, refillID string, in PurchaseInput) *Purchase {
	t.Helper()
	p, err := s.SubmitPurchase(refillID, in)
	check(t, "SubmitPurchase", err)
	return p
}

// runCycle drives one full empty-box -> submitted refill.
func runCycle(t *testing.T, s *Service, office Office, who string, in PurchaseInput) *Purchase {
	t.Helper()
	r := openRefill(t, s, office, who)
	claim(t, s, r.ID, who)
	return submit(t, s, r.ID, in)
}

// ---------------------------------------------------------------------------
// Fairness rotation: presence filter, visitor awareness, ranking, fallback.
// ---------------------------------------------------------------------------

func TestFairnessRotation(t *testing.T) {
	tests := []struct {
		name        string
		volunteers  []Volunteer
		prep        func(vs []Volunteer)
		wantDM      string // volunteer who must receive the refill DM
		mustNotDM   []string
		wantChannel bool // expect a fallback broadcast instead of a DM
	}{
		{
			name: "fewest 90d refills wins",
			volunteers: []Volunteer{
				vol("rpt", "Reporter", OfficeOslo, CurrencyNOK, OfficeOslo, true, 9),
				vol("anna", "Anna", OfficeOslo, CurrencyNOK, OfficeOslo, true, 1),
				vol("bor", "Bor", OfficeOslo, CurrencyNOK, OfficeOslo, true, 3),
			},
			wantDM: "anna",
		},
		{
			name: "tie broken by oldest last refill",
			volunteers: []Volunteer{
				vol("rpt", "Reporter", OfficeOslo, CurrencyNOK, OfficeOslo, true, 9),
				vol("anna", "Anna", OfficeOslo, CurrencyNOK, OfficeOslo, true, 1),
				vol("bor", "Bor", OfficeOslo, CurrencyNOK, OfficeOslo, true, 1),
			},
			prep: func(vs []Volunteer) {
				vs[1].LastRefillAt = testBase.Add(-10 * 24 * time.Hour) // anna refilled longer ago
				vs[2].LastRefillAt = testBase.Add(-2 * 24 * time.Hour)
			},
			wantDM: "anna",
		},
		{
			name: "WFH/holiday volunteers are never asked",
			volunteers: []Volunteer{
				vol("rpt", "Reporter", OfficeOslo, CurrencyNOK, OfficeOslo, true, 9),
				vol("anna", "Anna", OfficeOslo, CurrencyNOK, OfficeOslo, false, 0), // WFH today
				vol("bor", "Bor", OfficeOslo, CurrencyNOK, OfficeOslo, true, 2),
			},
			wantDM:    "bor",
			mustNotDM: []string{"anna"},
		},
		{
			name: "visitor from Bengaluru joins Oslo rotation; Bengaluru-side colleague excluded",
			volunteers: []Volunteer{
				vol("rpt", "Reporter", OfficeOslo, CurrencyNOK, OfficeOslo, true, 9),
				vol("priya", "Priya", OfficeBengaluru, CurrencyINR, OfficeOslo, true, 0),    // visiting Oslo
				vol("asha", "Asha", OfficeBengaluru, CurrencyINR, OfficeBengaluru, true, 0), // in Bengaluru
			},
			wantDM:    "priya",
			mustNotDM: []string{"asha"},
		},
		{
			name: "nobody eligible falls back to a channel broadcast",
			volunteers: []Volunteer{
				vol("rpt", "Reporter", OfficeOslo, CurrencyNOK, OfficeOslo, false, 0), // reporter WFH
			},
			wantChannel: true,
			mustNotDM:   []string{"rpt"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.prep != nil {
				tc.prep(tc.volunteers)
			}
			svc, chat, _, _ := newTestService(t)
			register(t, svc, tc.volunteers...)

			r := openRefill(t, svc, OfficeOslo, "rpt")
			got, err := svc.Refill(r.ID)
			check(t, "Refill", err)
			if got.Status != StatusOpen {
				t.Fatalf("fresh refill status = %s, want OPEN", got.Status)
			}

			if tc.wantChannel && len(chat.PostsTo("ch-oslo")) == 0 {
				t.Fatal("expected a fallback broadcast to ch-oslo, got none")
			}
			if tc.wantDM != "" {
				dms := chat.DMsTo(tc.wantDM)
				if len(dms) != 1 {
					t.Fatalf("expected exactly 1 DM to %s, got %d", tc.wantDM, len(dms))
				}
				if len(dms[0].Actions) != 2 || dms[0].Actions[0] != "[I'll buy it]" || dms[0].Actions[1] != "[Can't today]" {
					t.Fatalf("expected [I'll buy it]/[Can't today] actions, got %v", dms[0].Actions)
				}
			}
			for _, id := range tc.mustNotDM {
				if n := len(chat.DMsTo(id)); n != 0 {
					t.Fatalf("%s must not be DM'd, got %d messages", id, n)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 1. Concurrent double-claim prevention.
// ---------------------------------------------------------------------------

func TestConcurrentDoubleClaimPrevention(t *testing.T) {
	svc, _, _, _ := newTestService(t)

	vols := []Volunteer{vol("rpt", "Reporter", OfficeOslo, CurrencyNOK, OfficeOslo, true, 0)}
	const numClaimers = 32
	for i := 0; i < numClaimers; i++ {
		id := fmt.Sprintf("claimant-%02d", i)
		vols = append(vols, vol(id, id, OfficeOslo, CurrencyNOK, OfficeOslo, true, 1))
	}
	register(t, svc, vols...)

	r := openRefill(t, svc, OfficeOslo, "rpt")

	// Everyone races to claim the same OPEN refill at once.
	errs := make([]error, numClaimers)
	ready := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < numClaimers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-ready
			errs[i] = svc.ClaimRefill(r.ID, fmt.Sprintf("claimant-%02d", i))
		}(i)
	}
	close(ready)
	wg.Wait()

	successes := 0
	winner := ""
	for i, err := range errs {
		if err == nil {
			successes++
			winner = fmt.Sprintf("claimant-%02d", i)
			continue
		}
		if !errors.Is(err, ErrAlreadyClaimed) {
			t.Fatalf("claimant-%02d: got %v, want ErrAlreadyClaimed", i, err)
		}
	}
	if successes != 1 {
		t.Fatalf("exactly 1 concurrent claim must succeed, got %d", successes)
	}

	got, err := svc.Refill(r.ID)
	check(t, "Refill", err)
	if got.Status != StatusClaimed || got.ClaimedBy != winner {
		t.Fatalf("refill = %s claimed by %q, want CLAIMED by %q", got.Status, got.ClaimedBy, winner)
	}
}

// ---------------------------------------------------------------------------
// 2. Abandoned refill handoff (24h TTL sweeper).
// ---------------------------------------------------------------------------

func TestAbandonedRefillHandoff(t *testing.T) {
	svc, chat, _, clk := newTestService(t)

	register(t, svc,
		vol("olav", "Olav", OfficeOslo, CurrencyNOK, OfficeOslo, true, 1),  // initial claimer
		vol("nora", "Nora", OfficeOslo, CurrencyNOK, OfficeOslo, false, 0), // WFH: never asked
		vol("viv", "Vivian", OfficeOslo, CurrencyNOK, OfficeOslo, true, 2), // next fairest
	)

	r := openRefill(t, svc, OfficeOslo, "olav")
	if got := len(chat.DMsTo("olav")); got != 1 {
		t.Fatalf("initial DM to olav: got %d, want 1", got)
	}
	if n := len(chat.DMsTo("nora")); n != 0 {
		t.Fatalf("WFH volunteer must not be DM'd, got %d", n)
	}
	claim(t, svc, r.ID, "olav")

	clk.Advance(25 * time.Hour) // past the 24h TTL

	if n := svc.ProcessTimeouts(); n != 1 {
		t.Fatalf("ProcessTimeouts reclaimed %d refills, want 1", n)
	}

	got, err := svc.Refill(r.ID)
	check(t, "Refill", err)
	if got.Status != StatusOpen {
		t.Fatalf("expired claim status = %s, want OPEN", got.Status)
	}
	if len(got.SkippedVolunteers) != 1 || got.SkippedVolunteers[0] != "olav" {
		t.Fatalf("SkippedVolunteers = %v, want [olav]", got.SkippedVolunteers)
	}

	// Handoff: the next fairest *present* volunteer is asked. Nora (WFH) has
	// the lowest refill count but is skipped; Vivian gets the DM.
	if dms := chat.DMsTo("viv"); len(dms) != 1 {
		t.Fatalf("handoff DM to viv: got %d, want 1", len(dms))
	}
	if n := len(chat.DMsTo("nora")); n != 0 {
		t.Fatalf("WFH volunteer must still not be DM'd, got %d", n)
	}

	// Second abandonment: Vivian also lets it lapse -> everyone has skipped
	// -> last-resort channel broadcast.
	claim(t, svc, r.ID, "viv")
	clk.Advance(25 * time.Hour)
	if n := svc.ProcessTimeouts(); n != 1 {
		t.Fatalf("second ProcessTimeouts = %d, want 1", n)
	}
	got, err = svc.Refill(r.ID)
	check(t, "Refill", err)
	if len(got.SkippedVolunteers) != 2 || got.SkippedVolunteers[0] != "olav" || got.SkippedVolunteers[1] != "viv" {
		t.Fatalf("SkippedVolunteers = %v, want [olav viv]", got.SkippedVolunteers)
	}
	if posts := chat.PostsTo("ch-oslo"); len(posts) == 0 {
		t.Fatal("expected a channel broadcast when no volunteer is eligible")
	}
}

// ---------------------------------------------------------------------------
// 3. Cross-office visitor purchases + soft budget breach.
// ---------------------------------------------------------------------------

func TestCrossOfficeVisitorPurchase(t *testing.T) {
	svc, chat, hr, _ := newTestService(t)

	register(t, svc,
		vol("olav", "Olav", OfficeOslo, CurrencyNOK, OfficeOslo, true, 5),
		// Priya: Bengaluru payroll (INR), visiting Oslo for two weeks.
		vol("priya", "Priya", OfficeBengaluru, CurrencyINR, OfficeOslo, true, 0),
	)

	r := openRefill(t, svc, OfficeOslo, "olav")
	if dms := chat.DMsTo("priya"); len(dms) != 1 {
		t.Fatalf("visitor priya should be asked (fairest and present in Oslo), got %d DMs", len(dms))
	}
	claim(t, svc, r.ID, "priya")

	receipt := []byte("receipt-priya.jpg")
	p := submit(t, svc, r.ID, PurchaseInput{
		Amount:       800, // NOK: the host office's local currency, not her payroll INR
		Description:  "Friday fika for Oslo",
		Categories:   []Category{CategoryFruit},
		ShelfLife:    Shelf7Days,
		ReceiptImage: receipt,
	})

	if p.Currency != CurrencyNOK {
		t.Fatalf("purchase currency = %s, want NOK (office-local)", p.Currency)
	}
	if p.EmployeeID != "priya" || p.Amount != 800 {
		t.Fatalf("purchase = %s/%d, want priya/800", p.EmployeeID, p.Amount)
	}
	if p.FXRateToNOK != 1.0 || p.AmountNOK != 800 {
		t.Fatalf("NOK snapshot = %g/%g, want 1/800", p.FXRateToNOK, p.AmountNOK)
	}
	if p.OverBudget {
		t.Fatal("800 NOK must not breach the Oslo budget")
	}

	// The HR claim carries the office-local currency and her employee id.
	hrc, status, err := hr.Claim(p.HRClaimID)
	check(t, "hr.Claim", err)
	if status != HRStatusPending {
		t.Fatalf("HR status = %s, want PENDING", status)
	}
	if hrc.EmployeeID != "priya" {
		t.Fatalf("HR employee = %s, want priya (her own id)", hrc.EmployeeID)
	}
	if hrc.Currency != CurrencyNOK || hrc.Amount != 800 {
		t.Fatalf("HR claim = %s %d, want NOK 800 (office-local)", hrc.Currency, hrc.Amount)
	}
	if !bytes.Equal(hrc.ReceiptImage, receipt) {
		t.Fatal("receipt image must be forwarded to HR unchanged")
	}

	got, err := svc.Refill(r.ID)
	check(t, "Refill", err)
	if got.Status != StatusSubmitted {
		t.Fatalf("refill status = %s, want SUBMITTED", got.Status)
	}
}

func TestSoftBudgetBreach(t *testing.T) {
	svc, chat, hr, _ := newTestService(t)
	register(t, svc, vol("asha", "Asha", OfficeBengaluru, CurrencyINR, OfficeBengaluru, true, 0))

	r := openRefill(t, svc, OfficeBengaluru, "asha")
	claim(t, svc, r.ID, "asha")

	p := submit(t, svc, r.ID, PurchaseInput{
		Amount:      16000, // > the 15,000 INR monthly budget
		Description: "Monthly bulk restock",
		Categories:  []Category{CategoryNuts, CategorySavory},
		ShelfLife:   Shelf30Days,
	})

	if !p.OverBudget {
		t.Fatal("16,000 INR against a 15,000 INR budget must flag OverBudget")
	}
	if !p.OutsideTypicalRange {
		t.Fatal("16,000 INR is outside the 500-3,000 INR typical range and must be flagged")
	}

	// The office manager is alerted via chat.
	alerted := false
	for _, dm := range chat.DMsTo("mgr-blg") {
		if strings.Contains(dm.Text, "budget") {
			alerted = true
		}
	}
	if !alerted {
		t.Fatal("office manager must be alerted about the budget breach")
	}

	// The claim is still submitted to HR — reimbursement is never blocked.
	hrc, status, err := hr.Claim(p.HRClaimID)
	check(t, "hr.Claim", err)
	if hrc.Amount != 16000 || hrc.Currency != CurrencyINR {
		t.Fatalf("HR claim = %s %d, want INR 16000", hrc.Currency, hrc.Amount)
	}
	if status != HRStatusPending {
		t.Fatalf("HR status = %s, want PENDING (submitted)", status)
	}
	got, err := svc.Refill(r.ID)
	check(t, "Refill", err)
	if got.Status != StatusSubmitted {
		t.Fatalf("refill status = %s, want SUBMITTED (soft cap never blocks)", got.Status)
	}
}

// ---------------------------------------------------------------------------
// 4. Unified multi-currency monthly report + tag analytics.
// ---------------------------------------------------------------------------

func TestMonthlyReportAndTagAnalytics(t *testing.T) {
	svc, _, _, clk := newTestService(t)
	register(t, svc,
		vol("olav", "Olav", OfficeOslo, CurrencyNOK, OfficeOslo, true, 0),
		vol("asha", "Asha", OfficeBengaluru, CurrencyINR, OfficeBengaluru, true, 0),
	)

	// February purchase: must be excluded from the March report.
	clk.SetTo(testBase.AddDate(0, -1, 0)) // 2 Feb 2026
	runCycle(t, svc, OfficeOslo, "olav", PurchaseInput{
		Amount: 1000, Description: "february restock",
		Categories: []Category{CategoryFruit}, ShelfLife: Shelf7Days,
	})

	clk.SetTo(testBase) // 2 Mar 2026
	runCycle(t, svc, OfficeOslo, "olav", PurchaseInput{
		Amount: 800, Description: "fruit box",
		Categories: []Category{CategoryFruit}, ShelfLife: Shelf7Days,
	})
	runCycle(t, svc, OfficeOslo, "olav", PurchaseInput{
		Amount: 400, Description: "nuts and chocolate",
		Categories: []Category{CategoryNuts, CategoryChocolate}, ShelfLife: Shelf30Days,
	})
	runCycle(t, svc, OfficeBengaluru, "asha", PurchaseInput{
		Amount: 2000, Description: "week 1 snacks",
		Categories: []Category{CategoryDrinks}, ShelfLife: Shelf7Days,
	})
	runCycle(t, svc, OfficeBengaluru, "asha", PurchaseInput{
		Amount: 10000, Description: "bulk dry snacks",
		Categories: []Category{CategorySavory}, ShelfLife: Shelf30Days,
	})

	rep, err := svc.GenerateMonthlyReport(2026, time.March)
	check(t, "GenerateMonthlyReport", err)

	// Oslo: 1200 NOK (rate 1.0). Bengaluru: 12,000 INR at 0.085 -> 1020 NOK.
	if rep.TotalSpendNOK != 2220 {
		t.Fatalf("TotalSpendNOK = %g, want 2220", rep.TotalSpendNOK)
	}
	oslo := rep.ByOffice[OfficeOslo]
	if oslo == nil || oslo.LocalSpend != 1200 || oslo.Currency != CurrencyNOK || oslo.SpendNOK != 1200 || oslo.Purchases != 2 {
		t.Fatalf("Oslo report = %+v, want 1200 NOK across 2 purchases", oslo)
	}
	blg := rep.ByOffice[OfficeBengaluru]
	if blg == nil || blg.LocalSpend != 12000 || blg.Currency != CurrencyINR || blg.SpendNOK != 1020 || blg.Purchases != 2 {
		t.Fatalf("Bengaluru report = %+v, want 12,000 INR -> 1020 NOK across 2 purchases", blg)
	}

	// Tag analytics over March purchases only (February is excluded).
	wantCounts := map[Category]int{
		CategoryFruit: 1, CategoryNuts: 1, CategoryChocolate: 1,
		CategoryDrinks: 1, CategorySavory: 1,
	}
	for cat, want := range wantCounts {
		if got := rep.CategoryCounts[cat]; got != want {
			t.Fatalf("CategoryCounts[%s] = %d, want %d (February purchase must be excluded)", cat, got, want)
		}
	}
	// All tags tie at 1 -> alphabetical order.
	wantTop := []Category{CategoryChocolate, CategoryDrinks, CategoryFruit, CategoryNuts, CategorySavory}
	if len(rep.TopCategories) != len(wantTop) {
		t.Fatalf("TopCategories = %v, want %v", rep.TopCategories, wantTop)
	}
	for i := range wantTop {
		if rep.TopCategories[i] != wantTop[i] {
			t.Fatalf("TopCategories = %v, want %v", rep.TopCategories, wantTop)
		}
	}
	if len(rep.OverBudget) != 0 {
		t.Fatalf("OverBudget = %v, want none", rep.OverBudget)
	}

	// The February report contains only the February purchase.
	feb, err := svc.GenerateMonthlyReport(2026, time.February)
	check(t, "GenerateMonthlyReport(feb)", err)
	if feb.TotalSpendNOK != 1000 || feb.ByOffice[OfficeOslo].Purchases != 1 {
		t.Fatalf("February report = %g NOK / %+v, want 1000 NOK / 1 purchase",
			feb.TotalSpendNOK, feb.ByOffice[OfficeOslo])
	}
}

// ---------------------------------------------------------------------------
// 5. ATTR calculation, predictive nudge and expiry alerts.
// ---------------------------------------------------------------------------

func TestATTRAndExpiryAlerts(t *testing.T) {
	t.Run("ATTR calculation and predictive nudge", func(t *testing.T) {
		svc, chat, _, clk := newTestService(t)
		register(t, svc,
			vol("olav", "Olav", OfficeOslo, CurrencyNOK, OfficeOslo, true, 0),
			vol("viv", "Vivian", OfficeOslo, CurrencyNOK, OfficeOslo, true, 0),
		)

		// Default until history exists: 5 days.
		attr, err := svc.CalculateATTR(OfficeOslo)
		check(t, "CalculateATTR", err)
		if attr != 5*24*time.Hour {
			t.Fatalf("default ATTR = %s, want 120h", attr)
		}

		in := PurchaseInput{
			Amount: 400, Description: "weekly box",
			Categories: []Category{CategoryFruit}, ShelfLife: Shelf7Days,
		}
		runCycle(t, svc, OfficeOslo, "olav", in)
		clk.Advance(4 * 24 * time.Hour)
		runCycle(t, svc, OfficeOslo, "viv", in)
		clk.Advance(4 * 24 * time.Hour)
		runCycle(t, svc, OfficeOslo, "olav", in)

		// Two completed intervals of 4 days each -> ATTR = 96h.
		attr, err = svc.CalculateATTR(OfficeOslo)
		check(t, "CalculateATTR", err)
		if attr != 96*time.Hour {
			t.Fatalf("ATTR = %s, want 96h (two 4-day intervals)", attr)
		}

		// Quiet time beyond ATTR with no open cycle -> proactive nudge.
		clk.Advance(96*time.Hour + time.Hour)
		nudged := svc.RunATTRNudges()
		if len(nudged) != 1 || nudged[0] != OfficeOslo {
			t.Fatalf("RunATTRNudges = %v, want [oslo]", nudged)
		}
		// Fairness counters updated by completions: olav 2, viv 1 -> viv asked.
		if dms := chat.DMsTo("viv"); len(dms) == 0 {
			t.Fatal("proactive nudge must DM the fairest volunteer (viv)")
		}

		// The nudge must not double-fire while a cycle is already open.
		if again := svc.RunATTRNudges(); len(again) != 0 {
			t.Fatalf("second RunATTRNudges = %v, want none (cycle already open)", again)
		}
	})

	t.Run("expiry alerts", func(t *testing.T) {
		svc, chat, _, clk := newTestService(t)
		register(t, svc,
			vol("olav", "Olav", OfficeOslo, CurrencyNOK, OfficeOslo, true, 0),
			vol("asha", "Asha", OfficeBengaluru, CurrencyINR, OfficeBengaluru, true, 0),
		)

		// Bengaluru: perishable fruit + drinks with a 3-day shelf life.
		runCycle(t, svc, OfficeBengaluru, "asha", PurchaseInput{
			Amount: 2000, Description: "fresh fruit and lassi",
			Categories: []Category{CategoryFruit, CategoryDrinks}, ShelfLife: Shelf3Days,
		})
		// Oslo: non-perishable 30d+ stock never alerts.
		runCycle(t, svc, OfficeOslo, "olav", PurchaseInput{
			Amount: 1000, Description: "mixed nuts jar",
			Categories: []Category{CategoryNuts}, ShelfLife: Shelf30Days,
		})

		// Four days later: the Bengaluru fruit passed its shelf life with no
		// completed refill since -> EXPIRY alert; Oslo stays silent.
		clk.Advance(4 * 24 * time.Hour)
		alerts := svc.CheckExpiries()
		if len(alerts) != 1 || alerts[0].Office != OfficeBengaluru || alerts[0].Type != AlertExpiry {
			t.Fatalf("alerts = %+v, want exactly one EXPIRY alert for bengaluru", alerts)
		}
		if len(alerts[0].Categories) != 2 || alerts[0].Categories[0] != CategoryDrinks || alerts[0].Categories[1] != CategoryFruit {
			t.Fatalf("alert categories = %v, want [Drinks Fruit]", alerts[0].Categories)
		}
		if dms := chat.DMsTo("mgr-blg"); len(dms) != 1 || !strings.Contains(dms[0].Text, "shelf life") {
			t.Fatalf("manager DMs = %+v, want one expiry notice", dms)
		}

		// A completed, non-perishable refill rotates the box: the stale
		// Bengaluru purchase is suppressed and Oslo still never alerts.
		runCycle(t, svc, OfficeBengaluru, "asha", PurchaseInput{
			Amount: 1500, Description: "non-perishable rotation",
			Categories: []Category{CategoryNuts}, ShelfLife: Shelf30Days,
		})
		clk.Advance(27 * 24 * time.Hour) // 31 days since the Oslo purchase
		if alerts := svc.CheckExpiries(); len(alerts) != 0 {
			t.Fatalf("alerts = %+v, want none", alerts)
		}
	})
}

// ---------------------------------------------------------------------------
// 6. HR reimbursement sync.
// ---------------------------------------------------------------------------

func TestHRReimbursementSync(t *testing.T) {
	svc, chat, hr, _ := newTestService(t)
	register(t, svc, vol("olav", "Olav", OfficeOslo, CurrencyNOK, OfficeOslo, true, 0))

	// Cycle 1: submitted, then approved by HR.
	r1 := openRefill(t, svc, OfficeOslo, "olav")
	claim(t, svc, r1.ID, "olav")
	p1 := submit(t, svc, r1.ID, PurchaseInput{
		Amount: 600, Description: "week box",
		Categories: []Category{CategorySavory}, ShelfLife: Shelf7Days,
	})

	// Nothing decided yet: sync must be a no-op.
	n, err := svc.SyncHRReimbursements()
	check(t, "SyncHRReimbursements", err)
	if n != 0 {
		t.Fatalf("sync before approval reimbursed %d, want 0", n)
	}
	got, err := svc.Refill(r1.ID)
	check(t, "Refill", err)
	if got.Status != StatusSubmitted {
		t.Fatalf("refill status = %s, want SUBMITTED while HR is deciding", got.Status)
	}

	// HR approves -> sync flips the refill to REIMBURSED and DMs the volunteer.
	check(t, "hr.Approve", hr.Approve(p1.HRClaimID))
	n, err = svc.SyncHRReimbursements()
	check(t, "SyncHRReimbursements", err)
	if n != 1 {
		t.Fatalf("sync after approval reimbursed %d, want 1", n)
	}
	got, err = svc.Refill(r1.ID)
	check(t, "Refill", err)
	if got.Status != StatusReimbursed || got.ReimbursedAt.IsZero() {
		t.Fatalf("refill = %s (reimbursed at %v), want REIMBURSED with a timestamp", got.Status, got.ReimbursedAt)
	}
	dms := chat.DMsTo("olav")
	if !strings.Contains(dms[len(dms)-1].Text, "approved") {
		t.Fatalf("reimbursement DM = %q, want approval notice", dms[len(dms)-1].Text)
	}

	// A repeated sync must not double-transition or double-notify.
	n, err = svc.SyncHRReimbursements()
	check(t, "SyncHRReimbursements", err)
	if n != 0 {
		t.Fatalf("repeated sync reimbursed %d, want 0", n)
	}
	if got := len(chat.DMsTo("olav")); got != len(dms) {
		t.Fatalf("repeated sync sent duplicate DMs: %d -> %d", len(dms), got)
	}

	// Cycle 2: HR rejects -> the volunteer is told, the refill stays
	// SUBMITTED for HR follow-up.
	r2 := openRefill(t, svc, OfficeOslo, "olav")
	claim(t, svc, r2.ID, "olav")
	p2 := submit(t, svc, r2.ID, PurchaseInput{
		Amount: 350, Description: "top-up",
		Categories: []Category{CategoryDrinks}, ShelfLife: Shelf7Days,
	})
	check(t, "hr.Reject", hr.Reject(p2.HRClaimID))
	n, err = svc.SyncHRReimbursements()
	check(t, "SyncHRReimbursements", err)
	if n != 0 {
		t.Fatalf("sync with rejected claim reimbursed %d, want 0", n)
	}
	got, err = svc.Refill(r2.ID)
	check(t, "Refill", err)
	if got.Status != StatusSubmitted {
		t.Fatalf("rejected refill status = %s, want SUBMITTED", got.Status)
	}
	dms = chat.DMsTo("olav")
	if !strings.Contains(dms[len(dms)-1].Text, "rejected") {
		t.Fatalf("rejection DM = %q, want rejection notice", dms[len(dms)-1].Text)
	}
}
