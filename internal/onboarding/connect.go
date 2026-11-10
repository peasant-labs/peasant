package onboarding

import "slices"

// ConnectOffer is a village connection affordance at a boundary between stops.
// It is not a field, writes no configuration, and is never carried by a stop
// that a disconnected session hides.
type ConnectOffer struct {
	OfferAfter FlowKey
	When       []Condition
	Prompt     string
	Skip       string
}

// ConnectAnswer is the session-scoped record of the boundary offer. It is not
// a configuration field and never enters config.yaml. The village stop's own
// continue-locally answer does not set it: a person who skipped the connection
// at the start is asked again where a later publish's audience becomes concrete.
type ConnectAnswer uint8

const (
	ConnectUnanswered ConnectAnswer = iota
	ConnectSkipped
	ConnectConnected
)

// AllConnectAnswers is the closed set, including the unanswered zero value
// because that value is a real session state.
var AllConnectAnswers = []ConnectAnswer{ConnectUnanswered, ConnectSkipped, ConnectConnected}

// IsValid reports whether a is a known answer. The zero value is valid.
func (a ConnectAnswer) IsValid() bool { return slices.Contains(AllConnectAnswers, a) }

// String returns the stable token.
func (a ConnectAnswer) String() string {
	switch a {
	case ConnectUnanswered:
		return "unanswered"
	case ConnectSkipped:
		return "skipped"
	case ConnectConnected:
		return "connected"
	default:
		return "unknown"
	}
}

// NewConnectAnswer accepts unanswered, skipped, or connected.
func NewConnectAnswer(s string) (ConnectAnswer, error) {
	for _, answer := range AllConnectAnswers {
		if answer.String() == s {
			return answer, nil
		}
	}
	return ConnectUnanswered, actionable(
		"read connect answer "+s,
		s+" is not a boundary-offer answer; the closed set is unanswered, skipped, and connected",
		"onboarding.NewConnectAnswer",
		"a caller named what the person did with the boundary offer",
		"the answer was not accepted",
		"use unanswered, skipped, or connected")
}

// VillageConnectPrompt is the question shown before the first configuration
// section. Declining it does not answer the boundary offer.
func VillageConnectPrompt() string { return "connect to peasant online now?" }

// BoundaryConnectPrompt is the question shown when leaving the license section
// while still disconnected.
func BoundaryConnectPrompt() string {
	return "log in now to choose a default sharing visibility?"
}

func fullConnectOffers() []ConnectOffer {
	return []ConnectOffer{{
		OfferAfter: FlowLicense,
		When: []Condition{
			{Kind: CondVillageDisconnected},
			{Kind: CondLoginAvailable},
		},
		Prompt: BoundaryConnectPrompt(),
		Skip:   "no",
	}}
}

func offersFor(scope ModelScope) []ConnectOffer {
	if scope != ScopeFull {
		return nil
	}
	return fullConnectOffers()
}

// ConnectOffers returns the boundary offers this scope defines. The selection
// scope has none, because it has no license stop to leave.
func (m Model) ConnectOffers() []ConnectOffer {
	return append([]ConnectOffer(nil), m.offers...)
}

// ConnectOfferAfter reports the offer shown when leaving stop k. It is shown
// only when the offer's conditions hold and this offer has not been answered.
// A village-stop skip leaves ConnectAnswer unanswered, so the offer returns.
func (m Model) ConnectOfferAfter(k FlowKey, w WriteContext) (ConnectOffer, bool) {
	for _, offer := range m.offers {
		if offer.OfferAfter != k {
			continue
		}
		if !offer.visible(w) {
			continue
		}
		return offer, true
	}
	return ConnectOffer{}, false
}

func (o ConnectOffer) visible(w WriteContext) bool {
	if w.ConnectAnswer != ConnectUnanswered {
		return false
	}
	return ConditionsHold(o.When, w)
}
