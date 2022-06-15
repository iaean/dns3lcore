package acme

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dta4/dns3l-go/ca/common"
	"github.com/dta4/dns3l-go/ca/types"
	dns "github.com/dta4/dns3l-go/dns"
	dnscommon "github.com/dta4/dns3l-go/dns/common"
	dnstypes "github.com/dta4/dns3l-go/dns/types"
	"github.com/dta4/dns3l-go/util"
	"github.com/go-acme/lego/v4/certificate"
	legodns01 "github.com/go-acme/lego/v4/challenge/dns01"
)

const keyBitLength = 2048

//The Engine is created to have a consistent, object-based handle for Autokey operations
type Engine struct {
	Conf    *Config
	DNSConf *dns.Config
	State   ACMEStateManager
	CAState types.CAStateManager
}

//TriggerUpdate ensures that a key/certificate pair of the given line is available. It expects that the user
//is authenticated and authorized for the requested domain.
//It will look up the current state of the user and the key/certificate and ensures that the user and
//the requested key/cert is present.
func (e *Engine) TriggerUpdate(acmeuser string, keyname string, domains []string, dnsProviderID, email, issuedBy string) error {

	keyMustExist := acmeuser == "" || len(domains) <= 0

	for _, domain := range domains {
		err := dnscommon.ValidateDomainNameWildcard(domain)
		if err != nil {
			return err
		}
	}

	err := common.ValidateKeyName(keyname)
	if err != nil {
		return err
	}

	log.Infof("Trigger request by user '%s' for key '%s', domains '%s'", acmeuser, keyname, strings.Join(domains, ","))

	var domainsSanitized []string //Domains that came in new..
	if domains != nil {
		domainsSanitized, err = sanitizeDomains(domains)
		if err != nil {
			return err
		}
		log.Debugf("Domains '%s' validated and sanitized. Checking for existing keys/certificates...", strings.Join(domains, ","))
	}

	state, err := e.State.NewSession()
	if err != nil {
		return err
	}
	defer state.Close()

	castate, err := e.CAState.NewSession()
	if err != nil {
		return err
	}
	defer castate.Close()

	info, err := castate.GetCACertByID(keyname)
	if err != nil {
		return err
	}

	noKey := info == nil

	if !noKey {

		forceUpdate := false
		if len(domainsSanitized) > 0 && !util.StringSlicesEqual(info.Domains, domainsSanitized) {
			log.Warnf("Domains for key %s have changed from %v to %v, must force update.",
				keyname, info.Domains, domainsSanitized)
			info.Domains = domainsSanitized
			forceUpdate = true
		}
		if acmeuser != "" && acmeuser != info.ACMEUser {
			log.Warnf("ACME user for key %s has changed from %s to %s, must force update.",
				keyname, info.ACMEUser, acmeuser)
			info.ACMEUser = acmeuser
			forceUpdate = true
		}
		if issuedBy != "" && info.IssuedByUser != issuedBy {
			log.Infof("Issued-by-user for key %s has changed from %s to %s")
			info.IssuedByUser = issuedBy
		}

		now := time.Now()
		if !forceUpdate {
			renewalDate := info.ExpiryTime.AddDate(0, 0, -e.Conf.DaysRenewBeforeExpiry)
			if now.Before(renewalDate) {
				//Not yet due for renewal
				return &NoRenewalDueError{RenewalDate: renewalDate}
			}
			log.Infof("Key '%s' exists, cert is due for renewal", keyname)
		}
	}

	var privKey *rsa.PrivateKey
	if noKey {
		if keyMustExist {
			return &types.NotFoundError{}
		}
		info = &types.CACertInfo{
			ACMEUser:     acmeuser,
			Domains:      domainsSanitized,
			IssuedByUser: issuedBy,
		}
		log.Infof("Generating new RSA private key '%s' issued by user '%s'", keyname, acmeuser)
		privKey, err = generateRSAPrivateKey()
		if err != nil {
			return err
		}
		info.PrivKey = common.RSAPrivKeyToStr(privKey)
	} else {
		privKey, err = common.RSAPrivKeyFromStr(info.PrivKey)
		if err != nil {
			return err
		}
	}

	var u User = &DefaultUser{
		Config: e.Conf,
		State:  state,
		UID:    acmeuser,
		Email:  email,
	}

	err = u.InitUser()
	if err != nil {
		return err
	}

	dnsprovider, exists := e.DNSConf.Providers[dnsProviderID]
	if !exists {
		return fmt.Errorf("DNS provider 's' for setting ACME challenge has not been configured")
	}

	err = u.GetClient().Challenge.SetDNS01Provider(e.NewDNSProviderDNS3L(dnsprovider.Prov))
	if err != nil {
		return err
	}

	request := certificate.ObtainRequest{
		Domains:    info.Domains,
		PrivateKey: privKey,
		Bundle:     true,
	}
	log.Debugf("Requesting new certificate for key '%s', user '%s' via ACME",
		keyname, acmeuser)
	certificates, err := u.GetClient().Certificate.Obtain(request)
	if err != nil {
		return err
	}

	cert, err := parseCertificatePEM(certificates.Certificate)
	if err != nil {
		return err
	}
	if len(cert) <= 0 {
		return errors.New("no certs have been returned")
	}

	info.ValidStartTime = cert[0].NotBefore
	info.ExpiryTime = cert[0].NotAfter
	certStr := string(certificates.Certificate)
	issuerCertStr := string(certificates.IssuerCertificate)

	return castate.PutCACertData(!noKey, keyname, info,
		certStr, issuerCertStr)

}

func sanitizeDomains(domains []string) ([]string, error) {
	for i, d := range domains {
		if strings.HasSuffix(d, ".") {
			d = d[:len(d)-1]
			domains[i] = d
		}
	}
	return domains, nil
}

//NewDNSProviderOTC is a factory function for creating a new DNSProviderDNS3L
func (e *Engine) NewDNSProviderDNS3L(prov dnstypes.DNSProvider) *DNSProviderWrapper {
	return &DNSProviderWrapper{Provider: prov}
}

//The DNSProviderWrapper implements lego's DNS01 validation hook with acmeotc
type DNSProviderWrapper struct {
	Provider dnstypes.DNSProvider
}

// Present is called when the DNS01 challenge record shall be set up in the DNS.
// It wraps the acmeotc's DNS01 challenge setter into lego's DNS01 Present function.
func (p *DNSProviderWrapper) Present(domain, token, keyAuth string) error {
	fqdn, challenge := legodns01.GetRecord(domain, keyAuth)
	log.Debugf("Presenting challenge '%s', for domain '%s', fqdn '%s'...", challenge, domain, fqdn)
	err := p.Provider.SetRecordAcmeChallenge(domain, challenge)
	if err != nil {
		return err
	}
	log.Debugf("Presented challenge for domain '%s'", domain)
	return nil
}

// CleanUp is called when the DNS01 challenge record shall be set up in the DNS.
// It wraps the acmeotc's DNS01 challenge deleter into lego's DNS01 CleanUp function.
func (p *DNSProviderWrapper) CleanUp(domain, token, keyAuth string) error {
	log.Debugf("Cleaning up challenge for domain '%s'...", domain)
	err := p.Provider.DeleteRecordAcmeChallenge(domain)
	if err != nil {
		return err
	}
	log.Debugf("Cleaned up challenge for domain '%s'", domain)
	return nil
}

func generateRSAPrivateKey() (*rsa.PrivateKey, error) {
	k, err := rsa.GenerateKey(rand.Reader, keyBitLength)
	if err != nil {
		return nil, err
	}

	err = k.Validate()
	if err != nil {
		return nil, err
	}
	return k, nil
}

func parseCertificatePEM(certificate []byte) ([]*x509.Certificate, error) {
	result := make([]*x509.Certificate, 0)
	decodeTodo := certificate
	for {
		var block *pem.Block
		block, decodeTodo = pem.Decode(decodeTodo)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, err
			}
			result = append(result, cert)
		}
	}

	return result, nil

}
