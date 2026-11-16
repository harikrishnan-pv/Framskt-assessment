package snack

import "time"

// Message is a chat message, optionally carrying action buttons such as
// "[I'll buy it]" / "[Can't today]".
type Message struct {
	Text    string
	Actions []string
}

// ChatPort abstracts the internal chat tool. The service deliberately DMs a
// single accountable volunteer rather than broadcasting to a group channel;
// channels are used only for manager alerts and last-resort broadcasts.
// Implementations should be fast/non-blocking; sends are best-effort
// notifications and never fail the underlying state change.
type ChatPort interface {
	SendDM(userID string, msg Message) error
	PostChannel(channel string, msg Message) error
}

// HRClaim mirrors the external HR system's claim payload. Claims are
// submitted in the office's local currency; HR converts to the employee's
// payroll currency.
type HRClaim struct {
	EmployeeID   string
	Amount       int64
	Currency     Currency
	Description  string
	ReceiptImage []byte
}

// HRClaimStatus is the external HR workflow state. Approval takes 1-10 days.
type HRClaimStatus string

const (
	HRStatusPending  HRClaimStatus = "PENDING"
	HRStatusApproved HRClaimStatus = "APPROVED"
	HRStatusRejected HRClaimStatus = "REJECTED"
)

// HRPort abstracts the external HR reimbursement API.
type HRPort interface {
	// SubmitClaim registers a claim and returns HR's claim id.
	SubmitClaim(claim HRClaim) (claimID string, err error)
	// ClaimStatus polls the decision state of a submitted claim.
	ClaimStatus(claimID string) (HRClaimStatus, error)
}

// FXPort supplies purchase-time exchange rates into NOK, the unified
// reporting base currency.
type FXPort interface {
	RateToNOK(cur Currency, at time.Time) (float64, error)
}
