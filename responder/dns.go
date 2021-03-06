package responder

import (
	"crypto"
	"encoding/json"
	"fmt"

	"github.com/hlandau/acme/interaction"
)

type dnsResponder struct {
	validation []byte
	dnsString  string
}

func newDNSResponder(rcfg Config) (Responder, error) {
	s := &dnsResponder{}

	var err error
	s.validation, err = rcfg.responseJSON("dns-01")
	if err != nil {
		return nil, err
	}

	ka, err := rcfg.keyAuthorization()
	if err != nil {
		return nil, err
	}

	s.dnsString = b64enc(hashBytes([]byte(ka)))

	return s, nil
}

// Start is a no-op for the DNS method.
func (s *dnsResponder) Start(interactor interaction.Interactor) error {
	if interactor == nil {
		return fmt.Errorf("interaction func not provided but required")
	}

	log.Debug("dns-01 interaction prompt")
	_, err := interactor.Prompt(&interaction.Challenge{
		Title: "Verification DNS Record", Body: fmt.Sprintf(`All other verification methods have failed. In order to complete the DNS verification method, you must place the verification DNS record at

  _acme-challenge IN TXT %#v

under the name to be verified before continuing.

However, you should consider that it is likely to be easier for you to investigate and rectify the reason that the HTTP and TLSSNI challenges did not work. You may wish to consider this notice a failure condition.`, s.dnsString),
	})
	if err != nil {
		return err
	}

	return nil
}

// Stop is a no-op for the DNS method.
func (s *dnsResponder) Stop() error {
	log.Debug("dns-01 stopped")
	return nil
}

func (s *dnsResponder) RequestDetectedChan() <-chan struct{} {
	return nil
}

func (s *dnsResponder) Validation() json.RawMessage {
	return json.RawMessage(s.validation)
}

func (s *dnsResponder) ValidationSigningKey() crypto.PrivateKey {
	return nil
}

func init() {
	RegisterResponder("dns-01", newDNSResponder)
}

// © 2015 Hugo Landau <hlandau@devever.net>    MIT License
