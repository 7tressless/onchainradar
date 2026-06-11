// Package smartmoney provides the optional smart-money labeling provider for OCR's
// second detector ("a labeled smart-money wallet entered a tracked pool"). The signal is
// off by default and depends on an external service (Nansen), so it must never sit on the
// critical path: detection and attestation continue unaffected when no key is configured
// or the upstream is unreachable. The default Provider is therefore a no-op.
package smartmoney

import (
	"context"
	"errors"
	"net/http"
)

// Provider resolves whether an address is labeled as smart money. A failed lookup must
// return (false, "", err), never swallowed, so a caller can skip the signal rather than
// treat the address as unlabeled.
type Provider interface {
	IsSmartMoney(ctx context.Context, address string) (matched bool, label string, err error)
}

// Noop is the default Provider: it labels nothing, used whenever the smart-money signal
// is disabled or no credentials are configured.
type Noop struct{}

// NewNoop returns a Provider that always reports no match.
func NewNoop() Provider {
	return Noop{}
}

// IsSmartMoney always reports no match and no error.
func (Noop) IsSmartMoney(ctx context.Context, address string) (matched bool, label string, err error) {
	return false, "", nil
}

// NansenProvider labels addresses using Nansen's smart-money data. Stub: construction is
// wired but lookups are not yet implemented. Build one with NewNansen.
type NansenProvider struct {
	APIKey string       // authenticates requests to the Nansen API
	HTTP   *http.Client // nil => http.DefaultClient
}

// NewNansen returns a Nansen-backed Provider when apiKey is non-empty, else the no-op
// Provider, so a missing key keeps the signal disabled without callers branching.
func NewNansen(apiKey string) Provider {
	if apiKey == "" {
		return NewNoop()
	}
	return &NansenProvider{
		APIKey: apiKey,
		HTTP:   http.DefaultClient,
	}
}

// IsSmartMoney is not implemented yet and returns an error.
//
// TODO: call the Nansen smart-money / address-labels API, parse the label set, and map a
// hit to (true, label, nil). Until then it surfaces an explicit error rather than
// silently reporting no match, so a misconfigured-but-enabled provider stays visible.
func (p *NansenProvider) IsSmartMoney(ctx context.Context, address string) (matched bool, label string, err error) {
	return false, "", errors.New("smartmoney: nansen provider not implemented yet")
}
