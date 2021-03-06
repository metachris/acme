// Package storage implements the state directory specification and operations
// upon it.
package storage

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base32"
	"encoding/pem"
	"fmt"
	"github.com/hlandau/acme/acmeapi"
	"github.com/hlandau/acme/acmeutils"
	"github.com/hlandau/acme/fdb"
	"github.com/hlandau/acme/notify"
	"github.com/hlandau/acme/solver"
	"github.com/hlandau/xlog"
	"github.com/satori/go.uuid"
	"golang.org/x/net/context"
	"gopkg.in/yaml.v2"
	"io"
	"io/ioutil"
	"strings"
	"time"
)

var log, Log = xlog.New("acme.storage")

// Represents stored account data.
type Account struct {
	// N. Account private key.
	PrivateKey crypto.PrivateKey

	// N. Server directory URL.
	BaseURL string

	// Disposable. Authorizations.
	Authorizations map[string]*Authorization

	// ID: retrirved from BaseURL and PrivateKey.
	// Path: formed from ID.
	// Registration URL: can be recovered automatically.
}

// Returns the account ID (server URL/key ID).
func (a *Account) ID() string {
	accountID, err := determineAccountID(a.BaseURL, a.PrivateKey)
	log.Panice(err)

	return accountID
}

// Returns true iff the account is for a given provider URL.
func (a *Account) MatchesURL(p string) bool {
	return p == a.BaseURL
}

// Represents an authorization.
type Authorization struct {
	// N. The authorized hostname.
	Name string

	// N. The authorization URL.
	URL string

	// D. Can be derived from the URL. The authorization expiry time.
	Expires time.Time
}

func (a *Authorization) IsValid() bool {
	return time.Now().Before(a.Expires)
}

// Represents a stored target descriptor.
type Target struct {
	// N. List of SANs to place on any obtained certificate. May include
	// hostnames (and maybe one day SRV-IDs). May include wildcard hostnames.
	Names []string `yaml:"names,omitempty"`

	// N. Currently, this is the provider directory URL. An account matching it
	// will be used. At some point, a way to specify a particular account should
	// probably be added.
	Provider string `yaml:"provider,omitempty"`

	// D. Account to use, determined via Provider string.
	Account *Account `yaml:"-"`

	// N. Priority as a symlink target.
	Priority int `yaml:"priority,omitempty"`
}

func (t *Target) String() string {
	return fmt.Sprintf("Target(%s;%s;%d)", strings.Join(t.Names, ","), t.Provider, t.Priority)
}

// Represents stored certificate information.
type Certificate struct {
	// N. URL from which the certificate can be retrieved.
	URL string

	// D. Certificate data retrieved from URL, plus chained certificates.
	// The end certificate comes first, the root last, etc.
	Certificates [][]byte

	// D. True if the certificate has been downloaded.
	Cached bool

	// D. The private key for the certificate.
	Key *Key

	// D. ID: formed from hash of certificate URL.
	// D. Path: formed from ID.
}

func (c *Certificate) String() string {
	return fmt.Sprintf("Certificate(%v)", c.ID())
}

func (c *Certificate) ID() string {
	return determineCertificateID(c.URL)
}

// Represents a stored key.
type Key struct {
	// N. The key. Not kept in memory as there's no need to.

	// D. ID: Derived from the key itself.
	ID string

	// D. Path: formed from ID.
}

// ACME client store.
type Store struct {
	db *fdb.DB

	path                string
	referencedCerts     map[string]struct{}
	certs               map[string]*Certificate
	accounts            map[string]*Account
	keys                map[string]*Key
	targets             map[string]*Target
	defaultTarget       *Target // from conf
	defaultBaseURL      string
	webrootPaths        []string
	preferredRSAKeySize int
}

const RecommendedPath = "/var/lib/acme"

var storePermissions = []fdb.Permission{
	{Path: ".", DirMode: 0755, FileMode: 0644},
	{Path: "accounts", DirMode: 0700, FileMode: 0600},
	{Path: "desired", DirMode: 0755, FileMode: 0644},
	{Path: "live", DirMode: 0755, FileMode: 0644},
	{Path: "certs", DirMode: 0755, FileMode: 0644},
	{Path: "certs/*/haproxy", DirMode: 0700, FileMode: 0600}, // hack for HAProxy
	{Path: "keys", DirMode: 0700, FileMode: 0600},
	{Path: "conf", DirMode: 0755, FileMode: 0644},
	{Path: "tmp", DirMode: 0700, FileMode: 0600},
}

// Create a new client store using the given path.
func New(path string) (*Store, error) {
	if path == "" {
		path = RecommendedPath
	}

	db, err := fdb.Open(fdb.Config{
		Path:        path,
		Permissions: storePermissions,
	})
	if err != nil {
		return nil, err
	}

	s := &Store{
		db:             db,
		path:           path,
		defaultBaseURL: acmeapi.DefaultBaseURL,
	}

	err = s.load()
	if err != nil {
		return nil, err
	}

	return s, nil
}

func (s *Store) load() error {
	err := s.loadAccounts()
	if err != nil {
		return err
	}

	err = s.loadKeys()
	if err != nil {
		return err
	}

	err = s.loadCerts()
	if err != nil {
		return err
	}

	err = s.loadTargets()
	if err != nil {
		return err
	}

	err = s.linkTargets()
	if err != nil {
		return err
	}

	s.loadWebrootPaths()
	s.loadRSAKeySize()

	return nil
}

func (s *Store) loadAccounts() error {
	c := s.db.Collection("accounts")

	serverNames, err := c.List()
	if err != nil {
		return err
	}

	s.accounts = map[string]*Account{}
	for _, serverName := range serverNames {
		sc := c.Collection(serverName)

		accountNames, err := sc.List()
		if err != nil {
			return err
		}

		for _, accountName := range accountNames {
			ac := sc.Collection(accountName)

			err := s.validateAccount(serverName, accountName, ac)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *Store) validateAccount(serverName, accountName string, c *fdb.Collection) error {
	f, err := c.Open("privkey")
	if err != nil {
		return err
	}

	defer f.Close()

	b, err := ioutil.ReadAll(f)
	if err != nil {
		return err
	}

	pk, err := acmeutils.LoadPrivateKey(b)
	if err != nil {
		return err
	}

	f.Close()

	baseURL, err := decodeAccountURLPart(serverName)
	if err != nil {
		return err
	}

	account := &Account{
		PrivateKey:     pk,
		BaseURL:        baseURL,
		Authorizations: map[string]*Authorization{},
	}

	accountID := account.ID()
	actualAccountID := serverName + "/" + accountName
	if accountID != actualAccountID {
		return fmt.Errorf("account ID mismatch: %#v != %#v", accountID, actualAccountID)
	}

	s.accounts[accountID] = account

	err = s.validateAuthorizations(account, c)
	if err != nil {
		return err
	}

	return nil
}

func (s *Store) validateAuthorizations(account *Account, c *fdb.Collection) error {
	ac := c.Collection("authorizations")

	auths, err := ac.List()
	if err != nil {
		return err
	}

	for _, auth := range auths {
		auc := ac.Collection(auth)
		err := s.validateAuthorization(account, auth, auc)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) validateAuthorization(account *Account, authName string, c *fdb.Collection) error {
	ss, err := fdb.String(c.Open("expiry"))
	if err != nil {
		return err
	}

	expiry, err := time.Parse(time.RFC3339, strings.TrimSpace(ss))
	if err != nil {
		return err
	}

	azURL, _ := fdb.String(c.Open("url"))
	if !acmeapi.ValidURL(azURL) {
		azURL = ""
	}

	az := &Authorization{
		Name:    authName,
		URL:     strings.TrimSpace(azURL),
		Expires: expiry,
	}

	account.Authorizations[authName] = az
	return nil
}

func (s *Store) loadKeys() error {
	s.keys = map[string]*Key{}

	c := s.db.Collection("keys")

	keyIDs, err := c.List()
	if err != nil {
		return err
	}

	for _, keyID := range keyIDs {
		kc := c.Collection(keyID)

		err := s.validateKey(keyID, kc)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) validateKey(keyID string, kc *fdb.Collection) error {
	f, err := kc.Open("privkey")
	if err != nil {
		return err
	}

	defer f.Close()

	b, err := ioutil.ReadAll(f)
	if err != nil {
		return err
	}

	pk, err := acmeutils.LoadPrivateKey(b)
	if err != nil {
		return err
	}

	actualKeyID, err := determineKeyIDFromKey(pk)
	if err != nil {
		return err
	}

	if actualKeyID != keyID {
		return fmt.Errorf("key ID mismatch: %#v != %#v", keyID, actualKeyID)
	}

	k := &Key{
		ID: actualKeyID,
	}

	s.keys[actualKeyID] = k

	return nil
}

func (s *Store) loadCerts() error {
	s.certs = map[string]*Certificate{}

	c := s.db.Collection("certs")

	certIDs, err := c.List()
	if err != nil {
		return err
	}

	for _, certID := range certIDs {
		kc := c.Collection(certID)

		err := s.validateCert(certID, kc)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) validateCert(certID string, c *fdb.Collection) error {
	ss, err := fdb.String(c.Open("url"))
	if err != nil {
		return err
	}

	ss = strings.TrimSpace(ss)
	if !acmeapi.ValidURL(ss) {
		return fmt.Errorf("certificate has invalid URI")
	}

	actualCertID := determineCertificateID(ss)
	if certID != actualCertID {
		return fmt.Errorf("cert ID mismatch: %#v != %#v", certID, actualCertID)
	}

	crt := &Certificate{
		URL:          ss,
		Certificates: nil,
		Cached:       false,
	}

	fullchain, err := fdb.Bytes(c.Open("fullchain"))
	if err == nil {
		certs, err := acmeutils.LoadCertificates(fullchain)
		if err != nil {
			return err
		}

		xcrt, err := x509.ParseCertificate(certs[0])
		if err != nil {
			return err
		}

		keyID := determineKeyIDFromCert(xcrt)
		crt.Key = s.keys[keyID]

		if crt.Key != nil {
			err := c.WriteLink("privkey", fdb.Link{"keys/" + keyID + "/privkey"})
			if err != nil {
				return err
			}
		}

		crt.Certificates = certs
		crt.Cached = true
	}

	// TODO: obtain derived data
	s.certs[certID] = crt

	return nil
}

// Set the default provider directory URL.
func (s *Store) SetDefaultProvider(providerURL string) error {
	if !acmeapi.ValidURL(providerURL) {
		return fmt.Errorf("invalid provider URL")
	}

	s.defaultTarget.Provider = providerURL
	return s.saveDefaultTarget()
}

func (s *Store) saveDefaultTarget() error {
	confc := s.db.Collection("conf")

	b, err := yaml.Marshal(s.defaultTarget)
	if err != nil {
		return err
	}

	err = fdb.WriteBytes(confc, "target", b)
	if err != nil {
		return err
	}

	return nil
}

func (s *Store) loadTargets() error {
	s.targets = map[string]*Target{}

	// default target
	confc := s.db.Collection("conf")

	dtgt, err := s.validateTargetInner("target", confc)
	if err == nil {
		dtgt.Names = nil
		s.defaultTarget = dtgt
	} else {
		s.defaultTarget = &Target{}
	}

	// targets
	c := s.db.Collection("desired")

	desiredKeys, err := c.List()
	if err != nil {
		return err
	}

	for _, desiredKey := range desiredKeys {
		err := s.validateTarget(desiredKey, c)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) validateTarget(desiredKey string, c *fdb.Collection) error {
	tgt, err := s.validateTargetInner(desiredKey, c)
	if err != nil {
		return err
	}

	s.targets[desiredKey] = tgt
	return nil
}

func (s *Store) validateTargetInner(desiredKey string, c *fdb.Collection) (*Target, error) {
	b, err := fdb.Bytes(c.Open(desiredKey))
	if err != nil {
		return nil, err
	}

	tgt := &Target{}
	err = yaml.Unmarshal(b, tgt)
	if err != nil {
		return nil, err
	}

	if len(tgt.Names) == 0 {
		tgt.Names = []string{desiredKey}
	}

	for _, n := range tgt.Names {
		n = strings.ToLower(n)
		n = strings.TrimSuffix(n, ".")
		if !validHostname(n) {
			return nil, fmt.Errorf("invalid hostname in target %s: %s", desiredKey, n)
		}
	}

	tgt.Account, err = s.getAccountByProviderString(tgt.Provider)
	if err != nil {
		return nil, err
	}

	// TODO: tgt.Priority
	return tgt, nil
}

func (s *Store) EnsureRegistration() error {
	a, err := s.getAccountByProviderString("")
	if err != nil {
		return err
	}

	cl := s.getAccountClient(a)
	return solver.AssistedUpsertRegistration(cl, nil)
}

func (s *Store) getAccountByProviderString(p string) (*Account, error) {
	if p == "" && s.defaultTarget != nil {
		p = s.defaultTarget.Provider
	}

	if p == "" {
		p = acmeapi.DefaultBaseURL
	}

	if !acmeapi.ValidURL(p) {
		return nil, fmt.Errorf("provider URI is not a valid HTTPS URL")
	}

	for _, a := range s.accounts {
		if a.MatchesURL(p) {
			return a, nil
		}
	}

	return s.createNewAccount(p)
}

func (s *Store) createNewAccount(baseURL string) (*Account, error) {
	u, err := accountURLPart(baseURL)
	if err != nil {
		return nil, err
	}

	pk, keyID, err := s.createKey(s.db.Collection("accounts/" + u))
	if err != nil {
		return nil, err
	}

	a := &Account{
		PrivateKey: pk,
		BaseURL:    baseURL,
	}

	s.accounts[u+"/"+keyID] = a

	return a, nil
}

func (s *Store) createNewCertKey() (crypto.PrivateKey, *Key, error) {
	pk, keyID, err := s.createKey(s.db.Collection("keys"))
	if err != nil {
		return nil, nil, err
	}

	k := &Key{
		ID: keyID,
	}

	s.keys[keyID] = k

	return pk, k, nil
}

func (s *Store) createKey(c *fdb.Collection) (pk *rsa.PrivateKey, keyID string, err error) {
	pk, err = rsa.GenerateKey(rand.Reader, clampRSAKeySize(s.preferredRSAKeySize))
	if err != nil {
		return
	}

	keyID, err = s.saveKeyUnderID(c, pk)
	return
}

// Give a PEM-encoded key file, imports the key into the store. If the key is
// already installed, returns nil.
func (s *Store) ImportKey(r io.Reader) error {
	data, err := ioutil.ReadAll(r)
	if err != nil {
		return err
	}

	pk, err := acmeutils.LoadPrivateKey(data)
	if err != nil {
		return err
	}

	keyID, err := determineKeyIDFromKey(pk)
	if err != nil {
		return err
	}

	c := s.db.Collection("keys/" + keyID)

	f, err := c.Open("privkey")
	if err == nil {
		f.Close()
		return nil
	}

	ff, err := c.Create("privkey")
	if err != nil {
		return err
	}
	defer ff.CloseAbort()

	_, err = ff.Write(data)
	if err != nil {
		return err
	}

	ff.Close()
	return nil
}

// Given a certificate URL, imports the certificate into the store. The
// certificate will be retrirved on the next reconcile. If a certificate with
// that URL already exists, this is a no-op and returns nil.
func (s *Store) ImportCertificate(url string) error {
	certID := determineCertificateID(url)
	_, ok := s.certs[certID]
	if ok {
		return nil
	}

	return fdb.WriteBytes(s.db.Collection("certs/"+certID), "url", []byte(url))
}

// Given an account private key and the provider directory URL, imports that account key.
// If the account already exists and has a private key, this is a no-op and returns nil.
func (s *Store) ImportAccountKey(providerURL string, privateKey interface{}) error {
	accountID, err := determineAccountID(providerURL, privateKey)
	if err != nil {
		return err
	}

	_, ok := s.accounts[accountID]
	if ok {
		return nil
	}

	err = s.saveKey(s.db.Collection("accounts/"+accountID), privateKey)
	return err
}

// Saves a key as a file named "privkey" inside the given collection.
func (s *Store) saveKey(c *fdb.Collection, privateKey interface{}) error {
	var kb []byte

	switch v := privateKey.(type) {
	case *rsa.PrivateKey:
		kb = x509.MarshalPKCS1PrivateKey(v)
	default:
		return fmt.Errorf("unsupported private key type: %T", privateKey)
	}

	f, err := c.Create("privkey")
	if err != nil {
		return err
	}
	defer f.CloseAbort()

	err = pem.Encode(f, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: kb,
	})
	if err != nil {
		return err
	}

	f.Close()
	return nil
}

// Save a private key inside a key ID collection under the given collection.
func (s *Store) saveKeyUnderID(c *fdb.Collection, privateKey interface{}) (keyID string, err error) {
	keyID, err = determineKeyIDFromKey(privateKey)
	if err != nil {
		return
	}

	err = s.saveKey(c.Collection(keyID), privateKey)
	return
}

func (s *Store) linkTargets() error {
	names := map[string]*Target{}

	for _, tgt := range s.targets {
		for _, name := range tgt.Names {
			t2 := names[name]
			if targetGt(tgt, t2) {
				names[name] = tgt
			}
		}
	}

	var updatedHostnames []string

	for name, tgt := range names {
		c, err := s.findBestCertificateSatisfying(tgt)
		if err == nil {
			lt := "certs/" + c.ID()

			lnk, err := s.db.Collection("live").ReadLink(name)
			if err != nil || lnk.Target != lt {
				err = s.db.Collection("live").WriteLink(name, fdb.Link{Target: lt})
				if err != nil {
					return err
				}

				updatedHostnames = append(updatedHostnames, name)
			}
		}
	}

	err := notify.Notify("", s.path, updatedHostnames) // ignore error
	log.Errore(err, "failed to call notify hooks")

	return nil
}

// Runs the reconcilation operation and reloads state.
func (s *Store) Reconcile() error {
	err := s.reconcile()

	err2 := s.load()
	if err == nil {
		err = err2
	} else {
		log.Errore(err2, "failed to reload after reconciliation")
	}

	return err
}

// Error associated with a specific target, for clarity of error messages.
type TargetSpecificError struct {
	Target *Target
	Err    error
}

func (tse *TargetSpecificError) Error() string {
	return fmt.Sprintf("error satisfying target %v: %v", tse.Target, tse.Err)
}

type MultiError []error

func (me MultiError) Error() string {
	s := ""
	for _, e := range me {
		if s != "" {
			s += "; \n"
		}
		s += e.Error()
	}
	return "the following errors occurred:\n" + s
}

func (s *Store) reconcile() error {
	if s.haveUncachedCertificates() {
		log.Debug("there are uncached certificates - downloading them")

		err := s.downloadUncachedCertificates()
		if err != nil {
			return err
		}

		log.Debug("reloading after downloading uncached certificates")
		err = s.load()
		log.Debugf("finished reloading after downloading uncached certificates (%v)", err)
		if err != nil {
			return err
		}
		if s.haveUncachedCertificates() {
			log.Error("failed to download all uncached certificates")
			return fmt.Errorf("cannot obtain one or more uncached certificates")
		}
	}

	log.Debugf("now processing targets")
	var merr MultiError
	for _, t := range s.targets {
		c, err := s.findBestCertificateSatisfying(t)
		log.Debugf("best certificate satisfying %v is %v, err=%v", t, c, err)
		if err == nil && !s.certificateNeedsRenewing(c) {
			log.Debug("have best certificate which does not need renewing, skipping target")
			continue
		}

		log.Debugf("requesting certificate for target %v", t)
		err = s.requestCertificateForTarget(t)
		log.Errore(err, "failed to request certificate for target ", t)
		if err != nil {
			// do not block satisfaction of other targets just because one fails;
			// collect errors and return them as one
			merr = append(merr, &TargetSpecificError{
				Target: t,
				Err:    err,
			})
		}
	}
	log.Debugf("done processing targets, reconciliation complete, %d errors occurred", len(merr))

	if len(merr) != 0 {
		return merr
	}

	return nil
}

func (s *Store) haveUncachedCertificates() bool {
	for _, c := range s.certs {
		if !c.Cached {
			return true
		}
	}
	return false
}

func (s *Store) downloadUncachedCertificates() error {
	for _, c := range s.certs {
		if c.Cached {
			continue
		}

		err := s.downloadCertificate(c)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) downloadCertificate(c *Certificate) error {
	log.Debugf("downloading certificate %v", c)

	col := s.db.Collection("certs/" + c.ID())
	if col == nil {
		return fmt.Errorf("cannot get collection")
	}

	cl := acmeapi.Client{}

	crt := acmeapi.Certificate{
		URI: c.URL,
	}

	err := cl.WaitForCertificate(&crt, context.TODO())
	if err != nil {
		return err
	}

	if len(crt.Certificate) == 0 {
		return fmt.Errorf("nil certificate?")
	}

	fcert, err := col.Create("cert")
	if err != nil {
		return err
	}
	defer fcert.CloseAbort()

	fchain, err := col.Create("chain")
	if err != nil {
		return err
	}
	defer fchain.CloseAbort()

	ffullchain, err := col.Create("fullchain")
	if err != nil {
		return err
	}
	defer ffullchain.CloseAbort()

	err = pem.Encode(io.MultiWriter(fcert, ffullchain), &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: crt.Certificate,
	})
	if err != nil {
		return err
	}

	for _, ec := range crt.ExtraCertificates {
		err = pem.Encode(io.MultiWriter(fchain, ffullchain), &pem.Block{
			Type:  "CERTIFICATE",
			Bytes: ec,
		})
		if err != nil {
			return err
		}
	}

	fcert.Close()
	fchain.Close()
	ffullchain.Close()

	c.Certificates = nil
	c.Certificates = append(c.Certificates, crt.Certificate)
	c.Certificates = append(c.Certificates, crt.ExtraCertificates...)
	c.Cached = true

	return nil
}

func (s *Store) findBestCertificateSatisfying(t *Target) (*Certificate, error) {
	var bestCert *Certificate

	for _, c := range s.certs {
		if s.doesCertSatisfy(c, t) && (bestCert == nil || s.certBetterThan(c, bestCert)) {
			bestCert = c
		}
	}

	if bestCert == nil {
		return nil, fmt.Errorf("no certificate satisifes this target")
	}

	return bestCert, nil
}

func (s *Store) doesCertSatisfy(c *Certificate, t *Target) bool {
	if len(c.Certificates) == 0 {
		log.Debugf("certificate %v cannot satisfy %v because it has no actual certificates", c, t)
		return false
	}

	if c.Key == nil {
		// a certificate we don't have the key for is unusable.
		log.Debugf("certificate %v cannot satisfy %v because we do not have a key for it", c, t)
		return false
	}

	cc, err := x509.ParseCertificate(c.Certificates[0])
	if err != nil {
		log.Debugf("certificate %v cannot satisfy %v because we cannot parse it: %v", c, t, err)
		return false
	}

	names := map[string]struct{}{}
	for _, name := range cc.DNSNames {
		names[name] = struct{}{}
	}

	for _, name := range t.Names {
		_, ok := names[name]
		if !ok {
			log.Debugf("certificate %v cannot satisfy %v because required hostname %#v is not listed on it: %#v", c, t, name, cc.DNSNames)
			return false
		}
	}

	log.Debugf("certificate %v satisfies %v", c, t)
	return true
}

func (s *Store) certificateNeedsRenewing(c *Certificate) bool {
	if len(c.Certificates) == 0 {
		log.Debugf("not renewing %v because it has no actual certificates (???)", c)
		return false
	}

	cc, err := x509.ParseCertificate(c.Certificates[0])
	if err != nil {
		log.Debugf("not renewing %v because its end certificate is unparseable", c)
		return false
	}

	needsRenewing := cc.NotAfter.Before(time.Now().AddDate(0, 0, 30))
	log.Debugf("%v needsRenewing=%v notAfter=%v", c, needsRenewing, cc.NotAfter)
	return needsRenewing
}

func (s *Store) certBetterThan(a *Certificate, b *Certificate) bool {
	if len(a.Certificates) <= len(b.Certificates) || len(b.Certificates) == 0 {
		return false
	}

	ac, err := x509.ParseCertificate(a.Certificates[0])
	bc, err2 := x509.ParseCertificate(b.Certificates[0])
	if err != nil || err2 != nil {
		if err == nil && err2 != nil {
			return true
		}
		return false
	}

	return ac.NotAfter.After(bc.NotAfter)
}

func (s *Store) getAccountClient(a *Account) *acmeapi.Client {
	cl := &acmeapi.Client{}
	cl.AccountInfo.AccountKey = a.PrivateKey
	cl.BaseURL = a.BaseURL
	return cl
}

func (s *Store) getPriorKey(publicKey crypto.PublicKey) (crypto.PrivateKey, error) {
	// Returning an error here short circuits. If any errors occur, return (nil,nil).

	keyID, err := determineKeyIDFromPublicKey(publicKey)
	if err != nil {
		log.Errore(err, "failed to get key ID from public key")
		return nil, nil
	}

	if _, ok := s.keys[keyID]; !ok {
		log.Infof("failed to find key ID wanted by proofOfPossession: %s", keyID)
		return nil, nil // unknown key
	}

	c := s.db.Collection("keys/" + keyID)

	f, err := c.Open("privkey")
	if err != nil {
		log.Errore(err, "failed to open privkey for key with ID: ", keyID)
		return nil, nil
	}
	defer f.Close()

	b, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, err
	}

	privateKey, err := acmeutils.LoadPrivateKey(b)
	if err != nil {
		log.Errore(err, "failed to load private key for key with ID: ", keyID)
		return nil, nil
	}

	log.Infof("found key for proofOfPossession: %s", keyID)
	return privateKey, nil
}

func (s *Store) obtainAuthorization(name string, a *Account) error {
	cl := s.getAccountClient(a)

	az, err := solver.Authorize(cl, name, s.webrootPaths, nil, s.getPriorKey, context.TODO())
	if err != nil {
		return err
	}

	err = cl.LoadAuthorization(az)
	if err != nil {
		// Try proceeding anyway.
		return nil
	}

	c := s.db.Collection("accounts/" + a.ID() + "/authorizations/" + name)

	err = fdb.WriteBytes(c, "expiry", []byte(az.Expires.Format(time.RFC3339)))
	if err != nil {
		return err
	}

	err = fdb.WriteBytes(c, "url", []byte(az.URI))
	if err != nil {
		return err
	}

	saz := &Authorization{
		URL:     az.URI,
		Name:    az.Identifier.Value,
		Expires: az.Expires,
	}

	a.Authorizations[az.Identifier.Value] = saz

	return nil
}

func (s *Store) createCSR(t *Target) ([]byte, error) {
	csr := &x509.CertificateRequest{
		SignatureAlgorithm: x509.SHA256WithRSA,
		DNSNames:           t.Names,
	}

	pk, _, err := s.createNewCertKey()
	if err != nil {
		return nil, err
	}

	return x509.CreateCertificateRequest(rand.Reader, csr, pk)
}

func (s *Store) requestCertificateForTarget(t *Target) error {
	//return fmt.Errorf("not requesting certificate")
	cl := s.getAccountClient(t.Account)

	err := solver.AssistedUpsertRegistration(cl, nil)
	if err != nil {
		return err
	}

	authsNeeded, err := s.determineNecessaryAuthorizations(t)
	if err != nil {
		return err
	}

	for _, name := range authsNeeded {
		log.Debugf("trying to obtain authorization for %#v", name)
		err := s.obtainAuthorization(name, t.Account)
		if err != nil {
			log.Errore(err, "could not obtain authorization for ", name)
			return err
		}
	}

	csr, err := s.createCSR(t)
	if err != nil {
		return err
	}

	log.Debugf("requesting certificate for %v", t)
	acrt, err := cl.RequestCertificate(csr)
	if err != nil {
		log.Errore(err, "could not request certificate")
		return err
	}

	crt := &Certificate{
		URL: acrt.URI,
	}

	certID := crt.ID()

	c := s.db.Collection("certs/" + certID)

	err = fdb.WriteBytes(c, "url", []byte(crt.URL))
	if err != nil {
		log.Errore(err, "could not write certificate URL")
		return err
	}

	s.certs[certID] = crt

	log.Debugf("downloading certificate which was just requested: %#v", crt.URL)
	err = s.downloadCertificate(crt)
	if err != nil {
		return err
	}

	return nil
}

func (s *Store) determineNecessaryAuthorizations(t *Target) ([]string, error) {
	needed := map[string]struct{}{}
	for _, n := range t.Names {
		needed[n] = struct{}{}
	}

	a := t.Account
	for _, auth := range a.Authorizations {
		if auth.IsValid() {
			delete(needed, auth.Name)
		}
	}

	// preserve the order of the names in case the user considers that important
	var neededs []string
	for _, name := range t.Names {
		if _, ok := needed[name]; ok {
			neededs = append(neededs, name)
		}
	}

	return neededs, nil
}

func (s *Store) AddTarget(tgt Target) error {
	if len(tgt.Names) == 0 {
		return nil
	}

	for _, n := range tgt.Names {
		if !validHostname(n) {
			return fmt.Errorf("invalid hostname: %v", n)
		}
	}

	t := s.findTargetWithAllNames(tgt.Names)
	if t != nil {
		return nil
	}

	b, err := yaml.Marshal(&tgt)
	if err != nil {
		return err
	}

	c := s.db.Collection("desired")

	fn := s.makeUniqueTargetName(&tgt)
	return fdb.WriteBytes(c, fn, b)
}

func (s *Store) findTargetWithAllNames(names []string) *Target {
T:
	for _, t := range s.targets {
		for _, n := range names {
			if !containsName(t.Names, n) {
				continue T
			}
		}

		return t
	}
	return nil
}

func (s *Store) makeUniqueTargetName(tgt *Target) string {
	// Unfortunately we can't really check if the first hostname exists as a filename
	// and use another name instead as this would create all sorts of race conditions.
	// We have to use a random name.

	nprefix := ""
	if len(tgt.Names) > 0 {
		nprefix = tgt.Names[0] + "-"
	}

	b := uuid.NewV4().Bytes()
	str := strings.ToLower(strings.TrimRight(base32.StdEncoding.EncodeToString(b), "="))

	return nprefix + str
}

// © 2015 Hugo Landau <hlandau@devever.net>    MIT License
