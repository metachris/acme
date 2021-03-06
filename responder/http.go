package responder

import (
	"bytes"
	"crypto"
	"encoding/json"
	"fmt"
	"github.com/hlandau/acme/interaction"
	deos "github.com/hlandau/degoutils/os"
	"gopkg.in/tylerb/graceful.v1"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type httpResponder struct {
	serveMux            *http.ServeMux
	response            []byte
	requestDetectedChan chan struct{}
	stopFuncs           []func()
	ka                  []byte
	validation          []byte
	hostname            string
	webpaths            []string
	token               string
	filePath            string
	notifySupported     bool // is notify supported?
	listening           bool
}

func newHTTP(rcfg Config) (Responder, error) {
	s := &httpResponder{
		serveMux:            http.NewServeMux(),
		requestDetectedChan: make(chan struct{}, 1),
		hostname:            rcfg.Hostname,
		webpaths:            rcfg.WebPaths,
		token:               rcfg.Token,
		notifySupported:     true,
	}

	// Configure the HTTP server
	s.serveMux.HandleFunc("/.well-known/acme-challenge/"+rcfg.Token, s.handle)

	ka, err := rcfg.keyAuthorization()
	if err != nil {
		return nil, err
	}

	s.ka = []byte(ka)

	s.validation, err = rcfg.responseJSON("http-01")
	if err != nil {
		return nil, err
	}

	return s, nil
}

// HTTP handler.
func (s *httpResponder) handle(rw http.ResponseWriter, req *http.Request) {
	// Send the precomputed response.
	rw.Header().Set("Content-Type", "text/plain")
	rw.Write(s.ka)
	s.notify()
}

func (s *httpResponder) notify() {
	// Notify callers that a request has been detected.
	select {
	case s.requestDetectedChan <- struct{}{}:
	default:
	}
}

// Start handling HTTP requests.
func (s *httpResponder) Start(interactor interaction.Interactor) error {
	err := s.startListeners()
	if err != nil {
		return err
	}

	log.Debug("http-01 self test")
	err = s.selfTest()
	if err != nil {
		log.Infoe(err, "http-01 self test failed")
		s.Stop()
		return err
	}

	log.Debug("http-01 started")
	return nil
}

// Test that the challenge is reachable at the given hostname. If a hostname
// was not provided, this test is skipped.
func (s *httpResponder) selfTest() error {
	if s.hostname == "" {
		return nil
	}

	u := url.URL{
		Scheme: "http",
		Host:   s.hostname,
		Path:   "/.well-known/acme-challenge/" + s.token,
	}

	res, err := http.Get(u.String())
	if err != nil {
		return err
	}

	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("non-200 status code when doing self-test")
	}

	b, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return err
	}

	b = bytes.TrimSpace(b)
	if !bytes.Equal(b, s.ka) {
		return fmt.Errorf("got 200 response when doing self-test, but with the wrong data")
	}

	// If we detected a request, we support notifications, otherwise we don't.
	select {
	case <-s.requestDetectedChan:
	default:
		s.notifySupported = false
	}

	// Drain the notification channel in case we somehow made several requests.
L:
	for {
		select {
		case <-s.requestDetectedChan:
		default:
			break L
		}
	}

	return nil
}

// Tries to write a challenge file to each of the directories.
func webrootWriteChallenge(webroots map[string]struct{}, token string, ka []byte) {
	log.Debug("writing webroot challenge files")

	for wr := range webroots {
		os.MkdirAll(wr, 0755) // ignore errors
		fn := filepath.Join(wr, token)

		// Because /var/run/acme/acme-challenge may not exist due to /var/run
		// possibly being a tmpfs, and because that tmpfs is likely to be world
		// writable, there is a risk of following a maliciously crafted symlink to
		// cause a file to be overwritten as root. Open the file using a
		// no-symlinks flag if the OS supports it, but only for /var/run paths; we
		// want to support symlinks for other paths, which are presumably properly
		// controlled.
		//
		// Unfortunately earlier components in the pathname will still be followed
		// if they are symlinks, but it looks like this is the best we can do.
		var f *os.File
		var err error
		if strings.HasPrefix(wr, "/var/run/") {
			f, err = deos.OpenFileNoSymlinks(fn, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		} else {
			f, err = os.OpenFile(fn, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		}
		if err != nil {
			log.Errore(err, "failed to open webroot file ", fn)
			continue
		}

		f.Write(ka)
		f.Close()
	}
}

// Tries to remove a challenge file from each of the directories.
func webrootRemoveChallenge(webroots map[string]struct{}, token string) {
	for wr := range webroots {
		fn := filepath.Join(wr, token)

		log.Debugf("removing webroot file %s", fn)
		os.Remove(fn) // ignore errors
	}
}

func (s *httpResponder) getWebroots() map[string]struct{} {
	webroots := map[string]struct{}{}
	for _, p := range s.webpaths {
		if p != "" {
			webroots[strings.TrimRight(p, "/")] = struct{}{}
		}
	}

	// The webroot and redirector models both require us to drop the challenge at
	// a given path. If a webroot is not specified in the configuration, use an
	// ephemeral default that the redirector might be using anyway.
	webroots["/var/run/acme/acme-challenge"] = struct{}{}
	return webroots
}

func (s *httpResponder) startListeners() error {
	// Here's our brute force method: listen on everything that might work.
	s.startListener(":80")
	s.startListener("127.0.0.1:402")
	s.startListener("[::1]:402")
	s.startListener("127.0.0.1:4402")
	s.startListener("[::1]:4402")

	// Even if none of the listeners managed to start, the webroot or redirector
	// methods might work.
	webrootWriteChallenge(s.getWebroots(), s.token, s.ka)

	return nil
}

func (s *httpResponder) startListener(addr string) error {
	svr := &graceful.Server{
		NoSignalHandling: true,
		Server: &http.Server{
			Addr:    addr,
			Handler: s.serveMux,
		},
	}

	l, err := net.Listen("tcp", svr.Addr)
	if err != nil {
		log.Debuge(err, "failed to listen on ", svr.Addr)
		return err
	}

	go func() {
		defer l.Close()
		svr.Serve(l)
	}()

	stopFunc := func() {
		svr.Stop(10 * time.Millisecond)
		<-svr.StopChan()
	}

	s.stopFuncs = append(s.stopFuncs, stopFunc)
	return nil
}

// Stop handling HTTP requests.
func (s *httpResponder) Stop() error {
	var wg sync.WaitGroup
	wg.Add(len(s.stopFuncs))

	call := func(f func()) {
		defer wg.Done()
		f()
	}

	for _, f := range s.stopFuncs {
		go call(f)
	}
	wg.Wait()
	s.stopFuncs = nil

	// Try and remove challenges.
	webrootRemoveChallenge(s.getWebroots(), s.token)

	return nil
}

func (s *httpResponder) RequestDetectedChan() <-chan struct{} {
	if !s.notifySupported {
		return nil
	}

	return s.requestDetectedChan
}

func (s *httpResponder) Validation() json.RawMessage {
	return json.RawMessage(s.validation)
}

func (s *httpResponder) ValidationSigningKey() crypto.PrivateKey {
	return nil
}

func init() {
	RegisterResponder("http-01", newHTTP)
}

// © 2015 Hugo Landau <hlandau@devever.net>    MIT License
