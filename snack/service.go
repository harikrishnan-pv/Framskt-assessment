package snack

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tuning defaults.
const (
	// DefaultATTR is the predictive-nudge interval until an office has
	// enough completed-refill history: 5 days.
	DefaultATTR = 5 * 24 * time.Hour

	// DefaultClaimTTL is how long a CLAIMED refill may stay unpaid before
	// ProcessTimeouts hands it to the next volunteer: 24 hours.
	DefaultClaimTTL = 24 * time.Hour

	// attrWindow is the rolling window size for ATTR history.
	attrWindow = 10

	// fairnessWindow is the trailing window for refill fairness counts.
	fairnessWindow = 90 * 24 * time.Hour
)

// OfficeConfig carries per-office money and routing configuration.
type OfficeConfig struct {
	Currency      Currency
	MonthlyBudget int64  // office-local currency, whole units
	ManagerID     string // receives budget and expiry alerts
	Channel       string // office channel for last-resort broadcasts
	TypicalMin    int64  // typical purchase sanity range, whole units
	TypicalMax    int64
}

// Config configures a Service. Zero-valued durations fall back to the
// documented defaults.
type Config struct {
	Offices     map[Office]OfficeConfig
	DefaultATTR time.Duration // default: DefaultATTR (5 days)
	ClaimTTL    time.Duration // default: DefaultClaimTTL (24 hours)
}

// Service is the core business logic. Ports and the clock are injected, so
// the rules are fully deterministic under test. A single mutex serialises
// every mutation and read: claim locking is atomic and no state is touched
// outside the lock.
type Service struct {
	mu   sync.Mutex
	chat ChatPort
	hr   HRPort
	fx   FXPort
	cfg  Config
	now  func() time.Time

	volunteers map[string]*Volunteer
	refills    map[string]*Refill
	purchases  []*Purchase
	alerts     []Alert

	// completedAt[office] holds submission times of completed refills,
	// oldest first, trimmed to attrWindow entries: the rolling ATTR history.
	completedAt map[Office][]time.Time

	nextID int
}

// New creates a Service. hr and fx are required; a nil chat falls back to a
// no-op adapter.
func New(chat ChatPort, hr HRPort, fx FXPort, cfg Config) (*Service, error) {
	if hr == nil {
		return nil, errors.New("snack: HRPort is required")
	}
	if fx == nil {
		return nil, errors.New("snack: FXPort is required")
	}
	if chat == nil {
		chat = nopChat{}
	}
	if cfg.DefaultATTR <= 0 {
		cfg.DefaultATTR = DefaultATTR
	}
	if cfg.ClaimTTL <= 0 {
		cfg.ClaimTTL = DefaultClaimTTL
	}
	return &Service{
		chat:        chat,
		hr:          hr,
		fx:          fx,
		cfg:         cfg,
		now:         time.Now,
		volunteers:  map[string]*Volunteer{},
		refills:     map[string]*Refill{},
		completedAt: map[Office][]time.Time{},
	}, nil
}

// WithClock replaces the time source (used by tests for deterministic time
// travel). It returns the receiver so setup calls can chain.
func (s *Service) WithClock(now func() time.Time) *Service {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now != nil {
		s.now = now
	}
	return s
}

// RegisterVolunteer adds or replaces an employee. A missing active office or
// payroll currency defaults to the home office's values.
func (s *Service) RegisterVolunteer(v Volunteer) error {
	if v.ID == "" {
		return ErrInvalidVolunteer
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cfg.Offices[v.HomeOffice]; !ok {
		return ErrUnknownOffice
	}
	if v.ActiveOffice == "" {
		v.ActiveOffice = v.HomeOffice
	}
	if _, ok := s.cfg.Offices[v.ActiveOffice]; !ok {
		return ErrUnknownOffice
	}
	if v.PayrollCurrency == "" {
		v.PayrollCurrency = s.cfg.Offices[v.ActiveOffice].Currency
	}
	s.volunteers[v.ID] = &v
	return nil
}

// ReportEmpty handles the reactive QR trigger: it opens a refill cycle for
// the office and DMs the fairest eligible volunteer with accept/decline
// buttons.
func (s *Service) ReportEmpty(office Office, reporterID string) (*Refill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.cfg.Offices[office]; !ok {
		return nil, ErrUnknownOffice
	}
	if _, ok := s.volunteers[reporterID]; !ok {
		return nil, ErrUnknownVolunteer
	}

	r := &Refill{
		ID:        s.newIDLocked("rf"),
		Office:    office,
		Status:    StatusOpen,
		Trigger:   TriggerQR,
		CreatedAt: s.now(),
	}
	s.refills[r.ID] = r
	s.dmNextCandidateLocked(r, fmt.Sprintf(
		"The snack box in %s is empty (reported via QR). Can you buy snacks and refill it today?", office))
	return r, nil
}

// ClaimRefill atomically transitions an OPEN refill to CLAIMED on behalf of
// volunteerID. Concurrent claimers are rejected with ErrAlreadyClaimed.
func (s *Service) ClaimRefill(refillID, volunteerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.volunteers[volunteerID]; !ok {
		return ErrUnknownVolunteer
	}
	r, ok := s.refills[refillID]
	if !ok {
		return ErrRefillNotFound
	}
	if r.Status != StatusOpen {
		return fmt.Errorf("%w (status %s)", ErrAlreadyClaimed, r.Status)
	}

	now := s.now()
	r.Status = StatusClaimed
	r.ClaimedBy = volunteerID
	r.ClaimedAt = now
	r.ClaimExpiresAt = now.Add(s.cfg.ClaimTTL)
	return nil
}

// DeclineRefill records volunteerID's "[Can't today]" and hands the cycle to
// the next fairest eligible volunteer.
func (s *Service) DeclineRefill(refillID, volunteerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.volunteers[volunteerID]; !ok {
		return ErrUnknownVolunteer
	}
	r, ok := s.refills[refillID]
	if !ok {
		return ErrRefillNotFound
	}
	if r.Status != StatusOpen {
		return fmt.Errorf("%w: refill %s is %s, not OPEN", ErrInvalidState, r.ID, r.Status)
	}

	if !containsID(r.SkippedVolunteers, volunteerID) {
		r.SkippedVolunteers = append(r.SkippedVolunteers, volunteerID)
	}
	s.dmNextCandidateLocked(r, fmt.Sprintf(
		"The snack box in %s is still empty (a colleague declined). Can you refill it today?", r.Office))
	return nil
}

// ProcessTimeouts sweeps CLAIMED refills whose TTL lapsed: the claim reverts
// to OPEN, the stale claimer joins the cycle's skip list, and the next
// fairest eligible volunteer is DM'd. Returns the number of reclaimed
// cycles.
func (s *Service) ProcessTimeouts() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	n := 0
	for _, r := range s.refills {
		if r.Status != StatusClaimed || now.Before(r.ClaimExpiresAt) {
			continue
		}
		r.SkippedVolunteers = append(r.SkippedVolunteers, r.ClaimedBy)
		r.Status = StatusOpen
		r.ClaimedBy, r.ClaimedAt, r.ClaimExpiresAt = "", time.Time{}, time.Time{}
		s.dmNextCandidateLocked(r, fmt.Sprintf(
			"A snack refill claim in %s timed out without a receipt. Can you pick it up today?", r.Office))
		n++
	}
	return n
}

// PurchaseInput is the receipt payload a volunteer submits when the snacks
// are bought and the box is refilled.
type PurchaseInput struct {
	Amount       int64 // office-local currency, whole units
	Description  string
	Categories   []Category // 1-3 tags
	ShelfLife    ShelfLife  // one-tap preset; defaults to non-perishable
	ReceiptImage []byte
}

// SubmitPurchase completes a claimed refill: the claimant logs the receipt,
// the service snapshots the FX rate at purchase time, applies the soft
// budget guardrail (flag + manager alert, never a block), submits the claim
// to HR in the office's local currency, and advances CLAIMED -> SUBMITTED.
// It also records the completion for ATTR and refreshes fairness counters.
func (s *Service) SubmitPurchase(refillID string, in PurchaseInput) (*Purchase, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.refills[refillID]
	if !ok {
		return nil, ErrRefillNotFound
	}
	if r.Status != StatusClaimed {
		return nil, fmt.Errorf("%w: refill %s is %s, not CLAIMED", ErrInvalidState, r.ID, r.Status)
	}
	oc := s.cfg.Offices[r.Office]
	if in.Amount <= 0 {
		return nil, ErrInvalidAmount
	}
	if len(in.Categories) < 1 || len(in.Categories) > 3 {
		return nil, ErrInvalidCategories
	}
	if in.ShelfLife == "" {
		in.ShelfLife = Shelf30Days
	}

	now := s.now()
	rate, err := s.fx.RateToNOK(oc.Currency, now)
	if err != nil {
		return nil, fmt.Errorf("snack: fx snapshot failed: %w", err)
	}

	p := &Purchase{
		RefillID:     r.ID,
		Office:       r.Office,
		EmployeeID:   r.ClaimedBy,
		Amount:       in.Amount,
		Currency:     oc.Currency,
		Description:  in.Description,
		Categories:   append([]Category(nil), in.Categories...),
		ShelfLife:    in.ShelfLife,
		ReceiptImage: in.ReceiptImage,
		PurchasedAt:  now,
		FXRateToNOK:  rate,
		AmountNOK:    math.Round(float64(in.Amount) * rate),
	}

	// Soft budget guardrail: flag and alert the office manager, never
	// block — the volunteer has already spent personal money.
	if spend := s.monthlySpendLocked(r.Office, now); spend+in.Amount > oc.MonthlyBudget {
		p.OverBudget = true
		s.alertOverBudgetLocked(r.Office, spend, in.Amount, oc)
	}
	// Sanity flag only: HR audits the attached receipt image.
	if in.Amount < oc.TypicalMin || in.Amount > oc.TypicalMax {
		p.OutsideTypicalRange = true
	}

	claimID, err := s.hr.SubmitClaim(HRClaim{
		EmployeeID: p.EmployeeID,
		Amount:     p.Amount,
		Currency:   p.Currency, // office-local; HR converts to payroll currency
		Description: fmt.Sprintf("%s [refill %s, tags: %s, shelf-life %s]",
			in.Description, r.ID, joinCategories(p.Categories), p.ShelfLife),
		ReceiptImage: in.ReceiptImage,
	})
	if err != nil {
		return nil, fmt.Errorf("snack: hr claim submission failed: %w", err)
	}
	p.HRClaimID = claimID

	r.Purchase = p
	r.Status = StatusSubmitted
	r.SubmittedAt = now
	r.SkippedVolunteers = nil // cycle resolved; the skip list was cycle-scoped
	s.purchases = append(s.purchases, p)
	s.recordCompletionLocked(r.Office, p.EmployeeID, now)
	return p, nil
}

// SyncHRReimbursements polls HR for decided claims: approvals advance the
// refill SUBMITTED -> REIMBURSED and DM the volunteer (rejections notify the
// volunteer and leave the refill for HR follow-up). Returns the number of
// refills reimbursed in this pass.
func (s *Service) SyncHRReimbursements() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]string, 0, len(s.refills))
	for id := range s.refills {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic polling order

	n := 0
	for _, id := range ids {
		r := s.refills[id]
		if r.Status != StatusSubmitted || r.Purchase == nil || r.Purchase.HRClaimID == "" {
			continue
		}
		status, err := s.hr.ClaimStatus(r.Purchase.HRClaimID)
		if err != nil {
			return n, fmt.Errorf("snack: hr status poll failed (%s): %w", r.Purchase.HRClaimID, err)
		}
		switch status {
		case HRStatusApproved:
			r.Status = StatusReimbursed
			r.ReimbursedAt = s.now()
			payroll := ""
			if v, ok := s.volunteers[r.Purchase.EmployeeID]; ok {
				payroll = string(v.PayrollCurrency)
			}
			_ = s.chat.SendDM(r.Purchase.EmployeeID, Message{
				Text: fmt.Sprintf("HR approved your snack claim %s %d (%s). Reimbursement arrives in your %s payroll.",
					r.Purchase.Currency, r.Purchase.Amount, r.Purchase.HRClaimID, payroll),
			})
			n++
		case HRStatusRejected:
			_ = s.chat.SendDM(r.Purchase.EmployeeID, Message{
				Text: fmt.Sprintf("HR rejected snack claim %s — please follow up with HR directly.",
					r.Purchase.HRClaimID),
			})
		}
	}
	return n, nil
}

// RunATTRNudges applies the predictive trigger: for each office with no
// cycle OPEN or CLAIMED whose quiet time since the last completed refill
// exceeds its ATTR, it opens a refill cycle and DMs the fairest eligible
// volunteer. Returns the offices nudged (sorted, for determinism).
func (s *Service) RunATTRNudges() []Office {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	offices := make([]Office, 0, len(s.cfg.Offices))
	for o := range s.cfg.Offices {
		offices = append(offices, o)
	}
	sort.Slice(offices, func(i, j int) bool { return offices[i] < offices[j] })

	var nudged []Office
	for _, office := range offices {
		if s.hasActiveRefillLocked(office) {
			continue // a cycle is already in flight
		}
		h := s.completedAt[office]
		if len(h) == 0 {
			continue // no history yet: nothing to measure quiet time against
		}
		attr := s.attrLocked(office)
		if quiet := now.Sub(h[len(h)-1]); quiet <= attr {
			continue
		}
		r := &Refill{
			ID:        s.newIDLocked("rf"),
			Office:    office,
			Status:    StatusOpen,
			Trigger:   TriggerATTR,
			CreatedAt: now,
		}
		s.refills[r.ID] = r
		s.dmNextCandidateLocked(r, fmt.Sprintf(
			"It has been %.0f days since the %s snack box was last refilled (usually ~%.0f days). Please check whether the tray is empty.",
			now.Sub(h[len(h)-1]).Hours()/24, office, attr.Hours()/24))
		nudged = append(nudged, office)
	}
	return nudged
}

// CalculateATTR returns the office's Average Time To Refill: the mean
// interval between the last (up to) attrWindow completed refills. With fewer
// than two completions it reports the configured default (5 days).
func (s *Service) CalculateATTR(office Office) (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cfg.Offices[office]; !ok {
		return 0, ErrUnknownOffice
	}
	return s.attrLocked(office), nil
}

// CheckExpiries applies the freshness trigger: perishable purchases whose
// shelf life elapsed with no completed refill in that office since the
// purchase raise an EXPIRY alert (DM to the office manager) highlighting
// possibly slow-moving categories. Returns the alerts raised in this call.
func (s *Service) CheckExpiries() []Alert {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	expired := map[Office][]*Purchase{}
	for _, p := range s.purchases {
		if !p.ShelfLife.Perishable() {
			continue // "30d+" stock never expires
		}
		if now.Before(p.PurchasedAt.Add(p.ShelfLife.Duration())) {
			continue // still fresh
		}
		if s.rotatedSinceLocked(p.Office, p.PurchasedAt) {
			continue // a completed refill after the purchase implies rotation
		}
		expired[p.Office] = append(expired[p.Office], p)
	}

	offices := make([]Office, 0, len(expired))
	for o := range expired {
		offices = append(offices, o)
	}
	sort.Slice(offices, func(i, j int) bool { return offices[i] < offices[j] })

	var out []Alert
	for _, office := range offices {
		cats := uniqueCategories(expired[office])
		msg := fmt.Sprintf(
			"Expiry check for %s: %d perishable purchase(s) passed their shelf life without a refill since (tags: %s). Possibly slow-moving stock — please check the tray.",
			office, len(expired[office]), joinCategories(cats))
		alert := Alert{Type: AlertExpiry, Office: office, Message: msg, Categories: cats, At: now}
		s.alerts = append(s.alerts, alert)
		if oc := s.cfg.Offices[office]; oc.ManagerID != "" {
			_ = s.chat.SendDM(oc.ManagerID, Message{Text: msg})
		}
		out = append(out, alert)
	}
	return out
}

// MonthlyReport aggregates one calendar month across offices. LocalSpend is
// in the office's own currency; SpendNOK/TotalSpendNOK use the FX rate
// snapshotted on each purchase at purchase time.
type OfficeReport struct {
	LocalSpend int64
	Currency   Currency
	SpendNOK   float64
	Purchases  int
}

// MonthlyReport is management's unified monthly view.
type MonthlyReport struct {
	Year           int
	Month          time.Month
	TotalSpendNOK  float64
	ByOffice       map[Office]*OfficeReport
	CategoryCounts map[Category]int
	TopCategories  []Category // by count desc, then name asc
	OverBudget     []Office   // offices that breached the soft cap this month
}

// GenerateMonthlyReport builds the unified multi-currency report for a
// calendar month.
func (s *Service) GenerateMonthlyReport(year int, month time.Month) (*MonthlyReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	start := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)

	rep := &MonthlyReport{
		Year:           year,
		Month:          month,
		ByOffice:       map[Office]*OfficeReport{},
		CategoryCounts: map[Category]int{},
	}
	for office, oc := range s.cfg.Offices {
		rep.ByOffice[office] = &OfficeReport{Currency: oc.Currency}
	}

	seenOverBudget := map[Office]bool{}
	for _, p := range s.purchases {
		if p.PurchasedAt.Before(start) || !p.PurchasedAt.Before(end) {
			continue
		}
		or := rep.ByOffice[p.Office]
		if or == nil {
			or = &OfficeReport{Currency: p.Currency}
			rep.ByOffice[p.Office] = or
		}
		or.LocalSpend += p.Amount
		or.SpendNOK += p.AmountNOK
		or.Purchases++
		rep.TotalSpendNOK += p.AmountNOK
		for _, c := range p.Categories {
			rep.CategoryCounts[c]++
		}
		if p.OverBudget && !seenOverBudget[p.Office] {
			seenOverBudget[p.Office] = true
			rep.OverBudget = append(rep.OverBudget, p.Office)
		}
	}
	sort.Slice(rep.OverBudget, func(i, j int) bool { return rep.OverBudget[i] < rep.OverBudget[j] })
	rep.TopCategories = rankCategories(rep.CategoryCounts)
	return rep, nil
}

// Refill returns a copy of a refill for inspection.
func (s *Service) Refill(id string) (*Refill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.refills[id]
	if !ok {
		return nil, ErrRefillNotFound
	}
	cp := *r
	return &cp, nil
}

// Alerts returns a copy of every proactive alert raised so far.
func (s *Service) Alerts() []Alert {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Alert, len(s.alerts))
	copy(out, s.alerts)
	return out
}

// ---------------------------------------------------------------------------
// Internal helpers. All assume s.mu is held.
// ---------------------------------------------------------------------------

func (s *Service) newIDLocked(prefix string) string {
	s.nextID++
	return fmt.Sprintf("%s-%04d", prefix, s.nextID)
}

// attrLocked computes the rolling ATTR for an office.
func (s *Service) attrLocked(office Office) time.Duration {
	h := s.completedAt[office]
	if len(h) < 2 {
		return s.cfg.DefaultATTR
	}
	var total time.Duration
	for i := 1; i < len(h); i++ {
		total += h[i].Sub(h[i-1])
	}
	return total / time.Duration(len(h)-1)
}

// recordCompletionLocked appends a completion to the office's rolling ATTR
// history and refreshes the volunteer's fairness counters.
func (s *Service) recordCompletionLocked(office Office, volunteerID string, at time.Time) {
	h := append(s.completedAt[office], at)
	if len(h) > attrWindow {
		h = h[len(h)-attrWindow:]
	}
	s.completedAt[office] = h

	if v, ok := s.volunteers[volunteerID]; ok {
		v.RefillCount90d++ // nightly job recomputes the exact 90d value
		v.LastRefillAt = at
	}
}

// monthlySpendLocked sums this calendar month's office-local spend.
func (s *Service) monthlySpendLocked(office Office, now time.Time) int64 {
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	end := start.AddDate(0, 1, 0)
	var total int64
	for _, p := range s.purchases {
		if p.Office == office && !p.PurchasedAt.Before(start) && p.PurchasedAt.Before(end) {
			total += p.Amount
		}
	}
	return total
}

func (s *Service) alertOverBudgetLocked(office Office, spend, amount int64, oc OfficeConfig) {
	msg := fmt.Sprintf(
		"%s snack budget exceeded: month-to-date %d + claim %d = %d of the %d %s monthly budget. The claim was still submitted to HR.",
		office, spend, amount, spend+amount, oc.MonthlyBudget, oc.Currency)
	s.alerts = append(s.alerts, Alert{Type: AlertOverBudget, Office: office, Message: msg, At: s.now()})
	if oc.ManagerID != "" {
		_ = s.chat.SendDM(oc.ManagerID, Message{Text: msg})
	}
}

// rotatedSinceLocked reports whether office has a completed refill strictly
// after t (fresh stock implies the old stock was cleared).
func (s *Service) rotatedSinceLocked(office Office, t time.Time) bool {
	h := s.completedAt[office]
	return len(h) > 0 && h[len(h)-1].After(t)
}

func (s *Service) hasActiveRefillLocked(office Office) bool {
	for _, r := range s.refills {
		if r.Office == office && (r.Status == StatusOpen || r.Status == StatusClaimed) {
			return true
		}
	}
	return false
}

// eligibleLocked returns present volunteers active in the refill's office
// who have not skipped this cycle, fairest first: fewest refills in the
// trailing 90 days, ties broken by oldest LastRefillAt (never refilled
// sorts first), then by ID for determinism.
func (s *Service) eligibleLocked(r *Refill) []*Volunteer {
	var out []*Volunteer
	for _, v := range s.volunteers {
		if !v.IsPresent || v.ActiveOffice != r.Office || containsID(r.SkippedVolunteers, v.ID) {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RefillCount90d != out[j].RefillCount90d {
			return out[i].RefillCount90d < out[j].RefillCount90d
		}
		if !out[i].LastRefillAt.Equal(out[j].LastRefillAt) {
			return out[i].LastRefillAt.Before(out[j].LastRefillAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// dmNextCandidateLocked DMs the fairest eligible volunteer with the
// accept/decline buttons, or falls back to an office-channel broadcast when
// nobody is eligible. Chat sends are best-effort.
func (s *Service) dmNextCandidateLocked(r *Refill, text string) *Volunteer {
	cands := s.eligibleLocked(r)
	if len(cands) == 0 {
		if oc, ok := s.cfg.Offices[r.Office]; ok && oc.Channel != "" {
			_ = s.chat.PostChannel(oc.Channel, Message{
				Text: fmt.Sprintf(
					"The %s snack box is empty but no eligible volunteer is available right now. Please refill it if you can.",
					r.Office),
			})
		}
		return nil
	}
	c := cands[0]
	_ = s.chat.SendDM(c.ID, Message{
		Text:    text,
		Actions: []string{"[I'll buy it]", "[Can't today]"},
	})
	return c
}

// ---------------------------------------------------------------------------
// Small value helpers.
// ---------------------------------------------------------------------------

func containsID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func joinCategories(cats []Category) string {
	parts := make([]string, len(cats))
	for i, c := range cats {
		parts[i] = string(c)
	}
	return strings.Join(parts, ", ")
}

func uniqueCategories(purchases []*Purchase) []Category {
	seen := map[Category]bool{}
	var out []Category
	for _, p := range purchases {
		for _, c := range p.Categories {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func rankCategories(counts map[Category]int) []Category {
	out := make([]Category, 0, len(counts))
	for c := range counts {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if counts[out[i]] != counts[out[j]] {
			return counts[out[i]] > counts[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}
