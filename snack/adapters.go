package snack

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// In-memory stand-ins for the three ports. They double as test doubles and
// as the first deployment's bindings (swap for chat-webhook / HR-REST /
// rates-feed implementations later without touching the core).
// ---------------------------------------------------------------------------

// nopChat is the no-op chat adapter used when no ChatPort is supplied.
type nopChat struct{}

func (nopChat) SendDM(string, Message) error      { return nil }
func (nopChat) PostChannel(string, Message) error { return nil }

// SentDM records one direct message for assertions.
type SentDM struct {
	To      string
	Text    string
	Actions []string
}

// SentChannelPost records one channel post for assertions.
type SentChannelPost struct {
	Channel string
	Text    string
	Actions []string
}

// FakeChatAdapter is an in-memory ChatPort that records every DM and
// channel post so tests can assert who was notified and with which action
// buttons. Safe for concurrent use.
type FakeChatAdapter struct {
	mu    sync.Mutex
	dms   []SentDM
	posts []SentChannelPost
}

func (f *FakeChatAdapter) SendDM(userID string, msg Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dms = append(f.dms, SentDM{To: userID, Text: msg.Text, Actions: msg.Actions})
	return nil
}

func (f *FakeChatAdapter) PostChannel(channel string, msg Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, SentChannelPost{Channel: channel, Text: msg.Text, Actions: msg.Actions})
	return nil
}

// DMsTo returns copies of every DM sent to userID, in send order.
func (f *FakeChatAdapter) DMsTo(userID string) []SentDM {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []SentDM
	for _, dm := range f.dms {
		if dm.To == userID {
			out = append(out, dm)
		}
	}
	return out
}

// PostsTo returns copies of every post made to channel, in send order.
func (f *FakeChatAdapter) PostsTo(channel string) []SentChannelPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []SentChannelPost
	for _, p := range f.posts {
		if p.Channel == channel {
			out = append(out, p)
		}
	}
	return out
}

// fakeHRClaim is one claim stored by the fake HR system.
type fakeHRClaim struct {
	claim  HRClaim
	status HRClaimStatus
}

// FakeHRAdapter is an in-memory HRPort: every claim starts PENDING and
// stays there until a test calls Approve/Reject, mirroring HR's 1-10 day
// approval workflow. Safe for concurrent use.
type FakeHRAdapter struct {
	mu     sync.Mutex
	seq    int
	claims map[string]*fakeHRClaim
	order  []string
}

// NewFakeHRAdapter builds an empty fake HR system.
func NewFakeHRAdapter() *FakeHRAdapter {
	return &FakeHRAdapter{claims: map[string]*fakeHRClaim{}}
}

func (f *FakeHRAdapter) SubmitClaim(claim HRClaim) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("hr-%04d", f.seq)
	f.claims[id] = &fakeHRClaim{claim: claim, status: HRStatusPending}
	f.order = append(f.order, id)
	return id, nil
}

func (f *FakeHRAdapter) ClaimStatus(claimID string) (HRClaimStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.claims[claimID]
	if !ok {
		return "", errors.New("fakehr: unknown claim " + claimID)
	}
	return c.status, nil
}

// Approve simulates HR signing off on a claim.
func (f *FakeHRAdapter) Approve(claimID string) error {
	return f.setStatus(claimID, HRStatusApproved)
}

// Reject simulates HR rejecting a claim.
func (f *FakeHRAdapter) Reject(claimID string) error {
	return f.setStatus(claimID, HRStatusRejected)
}

func (f *FakeHRAdapter) setStatus(claimID string, status HRClaimStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.claims[claimID]
	if !ok {
		return errors.New("fakehr: unknown claim " + claimID)
	}
	c.status = status
	return nil
}

// Claim returns a snapshot of a stored claim and its status.
func (f *FakeHRAdapter) Claim(claimID string) (HRClaim, HRClaimStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.claims[claimID]
	if !ok {
		return HRClaim{}, "", errors.New("fakehr: unknown claim " + claimID)
	}
	return c.claim, c.status, nil
}

// StaticFXAdapter returns fixed rates into NOK. Suitable for tests and as a
// stub until a real rates feed is wired in.
type StaticFXAdapter struct {
	// Rates maps currency -> NOK per unit (e.g. INR: 0.085). NOK itself is
	// always 1.0 and needs no entry.
	Rates map[Currency]float64
}

func (a StaticFXAdapter) RateToNOK(cur Currency, at time.Time) (float64, error) {
	if cur == CurrencyNOK {
		return 1.0, nil
	}
	if r, ok := a.Rates[cur]; ok {
		return r, nil
	}
	return 0, fmt.Errorf("staticfx: no rate configured for %s", cur)
}
