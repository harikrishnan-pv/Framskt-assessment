// Package snack implements the core domain of the snack counter automation
// service: the refill lifecycle, fairness rotation, budget guardrails, HR
// reimbursement sync and unified multi-currency reporting. External systems
// (chat, HR, FX rates) are modelled as ports in ports.go so the business
// rules stay testable with in-memory adapters.
package snack

import (
	"errors"
	"time"
)

// Office identifies a physical office location.
type Office string

const (
	OfficeOslo      Office = "oslo"
	OfficeBengaluru Office = "bengaluru"
)

// Currency is an ISO-4217 code. Claims are submitted to HR in the office's
// local currency (HR converts to the employee's payroll currency); the
// unified monthly report aggregates purchases into NOK.
type Currency string

const (
	CurrencyNOK Currency = "NOK"
	CurrencyINR Currency = "INR"
)

// RefillStatus is the refill state machine:
//
//	OPEN -> CLAIMED -> SUBMITTED -> REIMBURSED
//
// A CLAIMED refill whose TTL lapses reverts to OPEN (see ProcessTimeouts).
type RefillStatus string

const (
	StatusOpen       RefillStatus = "OPEN"
	StatusClaimed    RefillStatus = "CLAIMED"
	StatusSubmitted  RefillStatus = "SUBMITTED"
	StatusReimbursed RefillStatus = "REIMBURSED"
)

// Trigger records why a refill cycle was opened.
type Trigger string

const (
	// TriggerQR is the reactive trigger: a laminated QR code taped inside
	// the box deep-links to the chat command "/snack empty <office>".
	TriggerQR Trigger = "QR_REACTIVE"

	// TriggerATTR is the predictive trigger: quiet time since the last
	// completed refill exceeded the office's Average Time To Refill.
	TriggerATTR Trigger = "ATTR_NUDGE"
)

// Category is a quick snack tag; every purchase carries 1-3 of them and the
// tags power the "which snacks actually get eaten" analytics.
type Category string

const (
	CategoryFruit     Category = "Fruit"
	CategoryNuts      Category = "Nuts"
	CategoryChocolate Category = "Chocolate"
	CategorySavory    Category = "Savory"
	CategoryDrinks    Category = "Drinks"
)

// ShelfLife is a one-tap shelf-life preset volunteers pick when logging a
// purchase. Only perishable presets raise EXPIRY alerts.
type ShelfLife string

const (
	Shelf3Days  ShelfLife = "3d"
	Shelf7Days  ShelfLife = "7d"
	Shelf30Days ShelfLife = "30d+" // non-perishable
)

// Duration returns the preset's shelf life; "30d+" is treated as 30 days.
func (s ShelfLife) Duration() time.Duration {
	switch s {
	case Shelf3Days:
		return 72 * time.Hour
	case Shelf7Days:
		return 7 * 24 * time.Hour
	default:
		return 30 * 24 * time.Hour
	}
}

// Perishable reports whether the preset can expire before the stock is
// cleared; long-lived "30d+" items never trigger freshness alerts.
func (s ShelfLife) Perishable() bool { return s != Shelf30Days }

// Volunteer is a company employee who can refill the snack box.
type Volunteer struct {
	ID              string // company login / HR employee id
	Name            string
	HomeOffice      Office
	PayrollCurrency Currency // HR reimburses in payroll currency

	// ActiveOffice is where the employee works today. Visitors spending
	// 1-2 weeks in the other office set it to the host office so they join
	// that office's refill rotation seamlessly.
	ActiveOffice Office

	// IsPresent is false on WFH days and holidays; absent volunteers are
	// excluded from refill rotation entirely.
	IsPresent bool

	// RefillCount90d is the cached fairness counter (completed refills in
	// the trailing 90 days). The service increments it when a refill
	// completes; in production a nightly job recomputes the exact
	// trailing-window value from history.
	RefillCount90d int

	// LastRefillAt is the last completed refill time (zero = never).
	LastRefillAt time.Time
}

// Refill is one empty-box cycle for an office.
type Refill struct {
	ID     string
	Office Office

	Status    RefillStatus
	Trigger   Trigger
	CreatedAt time.Time

	ClaimedBy      string
	ClaimedAt      time.Time
	ClaimExpiresAt time.Time // ClaimedAt + TTL; swept by ProcessTimeouts

	// SkippedVolunteers records people who declined or let their claim
	// lapse within this cycle so reassignment never asks them again. The
	// list is cycle-scoped and cleared once the refill completes.
	SkippedVolunteers []string

	SubmittedAt  time.Time
	ReimbursedAt time.Time
	Purchase     *Purchase
}

// Purchase is the receipt a volunteer logs when completing a refill.
type Purchase struct {
	RefillID     string
	Office       Office
	EmployeeID   string
	Amount       int64    // office-local currency, whole units
	Currency     Currency // office-local currency, submitted to HR as-is
	Description  string
	Categories   []Category // 1-3 tags
	ShelfLife    ShelfLife
	ReceiptImage []byte

	PurchasedAt time.Time
	FXRateToNOK float64 // snapshot at purchase time, for unified reporting
	AmountNOK   float64 // Amount * FXRateToNOK, rounded to whole NOK

	// OverBudget is a soft flag: the monthly budget was exceeded. It never
	// blocks the claim — the volunteer already spent personal money.
	OverBudget bool

	// OutsideTypicalRange is a sanity flag: the amount is outside the
	// office's typical purchase range (300-1200 NOK / 500-3000 INR).
	OutsideTypicalRange bool

	HRClaimID string
}

// AlertType marks the kind of proactive notification the service raised.
type AlertType string

const (
	AlertOverBudget AlertType = "OVER_BUDGET"
	AlertExpiry     AlertType = "EXPIRY_ALERT"
)

// Alert is one recorded proactive notification (budget breach, expiry).
type Alert struct {
	Type       AlertType
	Office     Office
	Message    string
	Categories []Category // expiry alerts: the affected snack tags
	At         time.Time
}

// Domain errors. Callers match with errors.Is.
var (
	ErrInvalidVolunteer  = errors.New("snack: volunteer requires an id")
	ErrUnknownOffice     = errors.New("snack: unknown office")
	ErrUnknownVolunteer  = errors.New("snack: unknown volunteer")
	ErrRefillNotFound    = errors.New("snack: refill not found")
	ErrAlreadyClaimed    = errors.New("snack: refill already claimed")
	ErrInvalidState      = errors.New("snack: invalid refill state for operation")
	ErrInvalidAmount     = errors.New("snack: amount must be positive")
	ErrInvalidCategories = errors.New("snack: purchase needs 1-3 category tags")
)
