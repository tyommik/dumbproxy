package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tg123/go-htpasswd"
	"golang.org/x/crypto/bcrypt"
)

const AUTH_REQUIRED_MSG = `<!DOCTYPE html>
<html>
<head>
<title>Welcome to nginx!</title>
<style>
    body {
        width: 35em;
        margin: 0 auto;
        font-family: Tahoma, Verdana, Arial, sans-serif;
    }
</style>
</head>
<body>
<h1>Welcome to nginx!</h1>
<p>If you see this page, the nginx web server is successfully installed and
working. Further configuration is required.</p>
<p>For online documentation and support please refer to
<a href="http://nginx.org/">nginx.org</a>.<br/>
Commercial support is available at
<a href="http://nginx.com/">nginx.com</a>.</p>
<p><em>Thank you for using nginx.</em></p>
</body>
</html>`
const BAD_REQ_MSG = "Bad Request\n"
const AUTH_TRIGGERED_MSG = "Browser auth triggered!\n"
const EPOCH_EXPIRE = "Thu, 01 Jan 1970 00:00:01 GMT"

type Auth interface {
	Validate(wr http.ResponseWriter, req *http.Request) (string, bool)
	Stop()
}

func NewAuth(paramstr string, logger *CondLogger) (Auth, error) {
	url, err := url.Parse(paramstr)
	if err != nil {
		return nil, err
	}

	switch strings.ToLower(url.Scheme) {
	case "static":
		return NewStaticAuth(url, logger)
	case "basicfile":
		return NewBasicFileAuth(url, logger)
	case "cert":
		return CertAuth{}, nil
	case "none":
		return NoAuth{}, nil
	default:
		return nil, errors.New("Unknown auth scheme")
	}
}

func NewStaticAuth(param_url *url.URL, logger *CondLogger) (*BasicAuth, error) {
	values, err := url.ParseQuery(param_url.RawQuery)
	if err != nil {
		return nil, err
	}
	username := values.Get("username")
	if username == "" {
		return nil, errors.New("\"username\" parameter is missing from auth config URI")
	}
	password := values.Get("password")
	if password == "" {
		return nil, errors.New("\"password\" parameter is missing from auth config URI")
	}
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		return nil, err
	}
	buf := bytes.NewBufferString(username)
	buf.WriteByte(':')
	buf.Write(hashedPassword)

	pwFile, err := htpasswd.NewFromReader(buf, htpasswd.DefaultSystems, func(parseError error) {
		logger.Error("static auth: password entry parse error: %v", err)
	})
	if err != nil {
		return nil, fmt.Errorf("can't instantiate pwFile: %w", err)
	}

	return &BasicAuth{
		hiddenDomain: strings.ToLower(values.Get("hidden_domain")),
		logger:       logger,
		pwFile:       pwFile,
		stopChan:     make(chan struct{}),
	}, nil
}

func requireBasicAuth(wr http.ResponseWriter, req *http.Request, hidden_domain string) {
	if hidden_domain != "" &&
		(subtle.ConstantTimeCompare([]byte(req.URL.Host), []byte(hidden_domain)) != 1 &&
			subtle.ConstantTimeCompare([]byte(req.Host), []byte(hidden_domain)) != 1) {
		http.Error(wr, BAD_REQ_MSG, http.StatusBadRequest)
	} else {
		wr.Header().Set("Proxy-Authenticate", `Basic realm="dumbproxy"`)
		wr.Header().Set("Content-Length", strconv.Itoa(len([]byte(AUTH_REQUIRED_MSG))))
		wr.WriteHeader(407)
		wr.Write([]byte(AUTH_REQUIRED_MSG))
	}
}

type BasicAuth struct {
	pwFilename   string
	pwFile       *htpasswd.File
	pwMux        sync.RWMutex
	logger       *CondLogger
	hiddenDomain string
	stopOnce     sync.Once
	stopChan     chan struct{}
	lastReloaded time.Time
}

func NewBasicFileAuth(param_url *url.URL, logger *CondLogger) (*BasicAuth, error) {
	values, err := url.ParseQuery(param_url.RawQuery)
	if err != nil {
		return nil, err
	}
	filename := values.Get("path")
	if filename == "" {
		return nil, errors.New("\"path\" parameter is missing from auth config URI")
	}

	auth := &BasicAuth{
		hiddenDomain: strings.ToLower(values.Get("hidden_domain")),
		pwFilename:   filename,
		logger:       logger,
		stopChan:     make(chan struct{}),
	}

	if err := auth.reload(); err != nil {
		return nil, fmt.Errorf("unable to load initial password list: %w", err)
	}

	reloadIntervalOption := values.Get("reload")
	reloadInterval, err := time.ParseDuration(reloadIntervalOption)
	if err != nil {
		reloadInterval = 0
	}
	if reloadInterval == 0 {
		reloadInterval = 15 * time.Second
	}
	if reloadInterval > 0 {
		go auth.reloadLoop(reloadInterval)
	}

	return auth, nil
}

func (auth *BasicAuth) reload() error {
	auth.logger.Info("reloading password file from %q...", auth.pwFilename)
	newPwFile, err := htpasswd.New(auth.pwFilename, htpasswd.DefaultSystems, func(parseErr error) {
		auth.logger.Error("failed to parse line in %q: %v", auth.pwFilename, parseErr)
	})
	if err != nil {
		return err
	}

	now := time.Now()

	auth.pwMux.Lock()
	auth.pwFile = newPwFile
	auth.lastReloaded = now
	auth.pwMux.Unlock()
	auth.logger.Info("password file reloaded.")

	return nil
}

func (auth *BasicAuth) condReload() error {
	reload := func() bool {
		pwFileModTime, err := fileModTime(auth.pwFilename)
		if err != nil {
			auth.logger.Warning("can't get password file modtime: %v", err)
			return true
		}
		return !pwFileModTime.Before(auth.lastReloaded)
	}()
	if reload {
		return auth.reload()
	}
	return nil
}

func (auth *BasicAuth) reloadLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-auth.stopChan:
			return
		case <-ticker.C:
			auth.condReload()
		}
	}
}

func (auth *BasicAuth) Validate(wr http.ResponseWriter, req *http.Request) (string, bool) {
	hdr := req.Header.Get("Proxy-Authorization")
	if hdr == "" {
		requireBasicAuth(wr, req, auth.hiddenDomain)
		return "", false
	}
	hdr_parts := strings.SplitN(hdr, " ", 2)
	if len(hdr_parts) != 2 || strings.ToLower(hdr_parts[0]) != "basic" {
		requireBasicAuth(wr, req, auth.hiddenDomain)
		return "", false
	}

	token := hdr_parts[1]
	data, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		requireBasicAuth(wr, req, auth.hiddenDomain)
		return "", false
	}

	pair := strings.SplitN(string(data), ":", 2)
	if len(pair) != 2 {
		requireBasicAuth(wr, req, auth.hiddenDomain)
		return "", false
	}

	login := pair[0]
	password := pair[1]

	auth.pwMux.RLock()
	pwFile := auth.pwFile
	auth.pwMux.RUnlock()

	if pwFile.Match(login, password) {
		if auth.hiddenDomain != "" &&
			(req.Host == auth.hiddenDomain || req.URL.Host == auth.hiddenDomain) {
			wr.Header().Set("Content-Length", strconv.Itoa(len([]byte(AUTH_TRIGGERED_MSG))))
			wr.Header().Set("Pragma", "no-cache")
			wr.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			wr.Header().Set("Expires", EPOCH_EXPIRE)
			wr.Header()["Date"] = nil
			wr.WriteHeader(http.StatusOK)
			wr.Write([]byte(AUTH_TRIGGERED_MSG))
			return "", false
		} else {
			return login, true
		}
	}
	requireBasicAuth(wr, req, auth.hiddenDomain)
	return "", false
}

func (auth *BasicAuth) Stop() {
	auth.stopOnce.Do(func() {
		close(auth.stopChan)
	})
}

type NoAuth struct{}

func (_ NoAuth) Validate(wr http.ResponseWriter, req *http.Request) (string, bool) {
	return "", true
}

func (_ NoAuth) Stop() {}

type CertAuth struct{}

func (_ CertAuth) Validate(wr http.ResponseWriter, req *http.Request) (string, bool) {
	if req.TLS == nil || len(req.TLS.VerifiedChains) < 1 || len(req.TLS.VerifiedChains[0]) < 1 {
		http.Error(wr, BAD_REQ_MSG, http.StatusBadRequest)
		return "", false
	} else {
		return req.TLS.VerifiedChains[0][0].Subject.String(), true
	}
}

func (_ CertAuth) Stop() {}
