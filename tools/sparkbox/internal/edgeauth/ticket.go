package edgeauth

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// TicketPrefix versions the handoff ticket's wire format, and — more
// importantly — domain-separates it from a session token.
//
// # Why a ticket exists at all
//
// A HiveMind sign-in code is single-use, and it is spent by the time the edge
// knows what it means. That is the right property for the code and an awkward
// one for the door: some handoffs must ask the visitor a question before they
// take effect (see handoff.go — a handoff that would swap an existing session,
// or create an account), and the answer arrives in a second request, by which
// time there is nothing left to redeem.
//
// So the redeemed facts have to survive one round trip. Storing them server-side
// would mean a map of pending sign-ins with an eviction policy, a lock, and a
// growth bound — state on a box that has so far needed none for authentication,
// because the whole edge session design is "the credential carries its own
// claims". A ticket keeps that property: the facts travel in the form, signed,
// and the edge remembers nothing.
//
// # Why it is not just a short-lived session token
//
// Because it is not a session and must never be usable as one. A ticket names
// an account the holder has NOT yet been signed in to — that is its entire
// purpose — so a value that Verify accepted would let the interstitial's hidden
// field be pasted into a cookie and skip the question the interstitial exists
// to ask. The prefix is part of the MAC input (see Signer.mac), so a ticket
// presented as a session fails the MAC, and a session presented as a ticket
// fails it too. There is no code path where the check can be forgotten, because
// the separation is in the signature rather than in a field somebody has to
// remember to read.
const TicketPrefix = "spk_t1."

// TicketTTL is how long a visitor has to answer the interstitial's question.
//
// Short because the ticket is a bearer credential for creating or switching
// into an account, and long enough that somebody who looks away from a page
// they were not expecting still gets to read it. Expiry is not the security
// boundary — the interstitial's question is — so this is a usability number
// with a safety floor, not the other way round.
const TicketTTL = 5 * time.Minute

// Ticket is a redeemed handoff, held across the interstitial.
//
// It carries exactly what the confirm step needs to finish the job it was
// already going to do, and nothing that would let the second request widen the
// first one's decision: Dest is already sanitised, Handle is already resolved,
// and Create is already decided. The confirm handler re-derives none of it,
// because re-deriving is where the two halves would get to disagree.
type Ticket struct {
	// Handle is the sparkbox account this handoff resolved to.
	Handle string `json:"h"`
	// Login is the GitHub login HiveMind vouched for, shown on the page and
	// passed to the admission so the account half never reads it off a form.
	Login string `json:"l"`
	// Email is optional, recorded on a created account.
	Email string `json:"e,omitempty"`
	// Dest is the already-sanitised landing URL.
	Dest string `json:"d"`
	// Create records that the account did not exist when the handoff was
	// redeemed. It drives the page's wording and nothing else — the admission
	// is idempotent, so a race that creates the account in between is harmless.
	Create bool `json:"c,omitempty"`
	// Expiry is the unix second after which this is refused.
	Expiry int64 `json:"exp"`
}

// MintTicket signs t for TicketTTL.
func (s *Signer) MintTicket(t Ticket) (string, error) {
	if t.Handle == "" {
		return "", fmt.Errorf("handoff ticket needs a handle")
	}
	t.Expiry = time.Now().UTC().Add(TicketTTL).Unix()
	payload, err := marshalTicket(t)
	if err != nil {
		return "", err
	}
	return TicketPrefix + payload + "." + s.mac(TicketPrefix, payload), nil
}

// marshalTicket renders a ticket's signed half. Split out so the mint path and
// the test that needs to forge an expiry agree on the encoding by construction.
func marshalTicket(t Ticket) (string, error) {
	body, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(body), nil
}

// VerifyTicket checks a ticket's MAC and expiry and returns what it carries.
func (s *Signer) VerifyTicket(value string) (Ticket, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(value), TicketPrefix)
	if !ok {
		return Ticket{}, false
	}
	payload, sig, ok := strings.Cut(rest, ".")
	if !ok {
		return Ticket{}, false
	}
	if subtle.ConstantTimeCompare([]byte(sig), []byte(s.mac(TicketPrefix, payload))) != 1 {
		return Ticket{}, false
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Ticket{}, false
	}
	var t Ticket
	if err := json.Unmarshal(body, &t); err != nil {
		return Ticket{}, false
	}
	if t.Handle == "" || time.Now().Unix() >= t.Expiry {
		return Ticket{}, false
	}
	return t, true
}
