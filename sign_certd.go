package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cloudtools/ssh-cert-authority/util"
	"github.com/codegangsta/cli"
	"github.com/gorilla/mux"
	"golang.org/x/crypto/ssh"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"time"
)

type certRequest struct {
	// This struct tracks state for certificate requests. Imagine this one day
	// being stored in a persistent data store.
	request     *ssh.Certificate
	submitTime  time.Time
	environment string
	signatures  map[string]bool
	certSigned  bool
	reason      string
}

func newcertRequest() certRequest {
	var cr certRequest
	cr.submitTime = time.Now()
	cr.certSigned = false
	cr.signatures = make(map[string]bool)
	return cr
}

type certRequestHandler struct {
	Config       map[string]ssh_ca_util.SignerdConfig
	state        map[string]certRequest
	sshAgentConn io.ReadWriter
	NextSerial   chan uint64
}

type signingRequest struct {
	config      *ssh_ca_util.SignerdConfig
	environment string
	cert        *ssh.Certificate
}

func (h *certRequestHandler) formBoilerplate(req *http.Request) (*ssh_ca_util.SignerdConfig, string, error) {
	err := req.ParseForm()
	if err != nil {
		err := fmt.Errorf("%v", err)
		return nil, "", err
	}
	if req.Form["environment"] == nil {
		err := errors.New("Must specify environment")
		return nil, "", err
	}
	environment := req.Form["environment"][0]
	config, ok := h.Config[environment]
	if !ok {
		err := errors.New("Environment is not configured (is it valid?)")
		return nil, "", err
	}
	return &config, environment, nil
}

func (h *certRequestHandler) createSigningRequest(rw http.ResponseWriter, req *http.Request) {
	config, environment, err := h.formBoilerplate(req)
	if err != nil {
		http.Error(rw, fmt.Sprintf("%v", err), http.StatusBadRequest)
		return
	}
	cert, err := h.extractCertFromRequest(req, config.AuthorizedUsers)
	if err != nil {
		http.Error(rw, fmt.Sprintf("%v", err), http.StatusBadRequest)
		return
	}

	if req.Form["reason"][0] == "" {
		http.Error(rw, "You forgot to send in a reason", http.StatusBadRequest)
		return
	}
	reason := req.Form["reason"][0]

	requestID := make([]byte, 10)
	rand.Reader.Read(requestID)
	requestIDStr := base32.StdEncoding.EncodeToString(requestID)
	nextSerial := <-h.NextSerial

	err = h.saveSigningRequest(config, environment, reason, requestIDStr, nextSerial, cert)
	if err != nil {
		http.Error(rw, fmt.Sprintf("Request not made: %v", err), http.StatusBadRequest)
		return
	}

	requesterFp := ssh_ca_util.MakeFingerprint(cert.SignatureKey.Marshal())
	log.Printf("Cert request serial %d id %s env %s from %s (%s) @ %s principals %v valid from %d to %d for '%s'\n",
		cert.Serial, requestIDStr, environment, requesterFp, config.AuthorizedUsers[requesterFp],
		req.RemoteAddr, cert.ValidPrincipals, cert.ValidAfter, cert.ValidBefore, reason)
	rw.WriteHeader(http.StatusCreated)
	rw.Write([]byte(requestIDStr))

	return
}

func (h *certRequestHandler) saveSigningRequest(config *ssh_ca_util.SignerdConfig, environment, reason, requestIDStr string, requestSerial uint64, cert *ssh.Certificate) error {
	requesterFp := ssh_ca_util.MakeFingerprint(cert.SignatureKey.Marshal())

	// We override keyid here so that its a server controlled value. Instead of
	// letting a requester attempt to spoof it.
	var ok bool
	cert.KeyId, ok = config.AuthorizedUsers[requesterFp]
	if !ok {
		return fmt.Errorf("Requester fingerprint (%s) not found in config", requesterFp)
	}

	if requestSerial == 0 {
		return fmt.Errorf("Serial number not set.")
	}
	cert.Serial = requestSerial

	certRequest := newcertRequest()
	certRequest.request = cert
	if environment == "" {
		return fmt.Errorf("Environment is a required field")
	}
	certRequest.environment = environment

	if reason == "" {
		return fmt.Errorf("Reason is a required field")
	}
	certRequest.reason = reason

	if len(requestIDStr) < 12 {
		return fmt.Errorf("Request id is too short to be useful.")
	}
	_, ok = h.state[requestIDStr]
	if ok {
		return fmt.Errorf("Request id '%s' already in use.", requestIDStr)
	}
	h.state[requestIDStr] = certRequest

	return nil
}

func (h *certRequestHandler) extractCertFromRequest(req *http.Request, authorizedSigners map[string]string) (*ssh.Certificate, error) {

	if req.PostForm["cert"] == nil || len(req.PostForm["cert"]) == 0 {
		err := errors.New("Please specify exactly one cert request")
		return nil, err
	}

	rawCertRequest, err := base64.StdEncoding.DecodeString(req.PostForm["cert"][0])
	if err != nil {
		err := errors.New("Unable to base64 decode cert request")
		return nil, err
	}
	pubKey, err := ssh.ParsePublicKey(rawCertRequest)
	if err != nil {
		err := errors.New("Unable to parse cert request")
		return nil, err
	}

	cert := pubKey.(*ssh.Certificate)

	var certChecker ssh.CertChecker
	certChecker.IsAuthority = func(auth ssh.PublicKey) bool {
		fingerprint := ssh_ca_util.MakeFingerprint(auth.Marshal())
		_, ok := authorizedSigners[fingerprint]
		return ok
	}
	err = certChecker.CheckCert(cert.ValidPrincipals[0], cert)
	if err != nil {
		err := fmt.Errorf("Cert not valid: %v", err)
		return nil, err
	}
	return cert, nil
}

type listResponseElement struct {
	Environment string
	Reason      string
	CertBlob    string
}
type certRequestResponse map[string]listResponseElement

func newResponseElement(environment string, reason string, certBlob string) listResponseElement {
	var element listResponseElement
	element.Environment = environment
	element.Reason = reason
	element.CertBlob = certBlob
	return element
}

func (h *certRequestHandler) listPendingRequests(rw http.ResponseWriter, req *http.Request) {
	var certRequestID string

	err := req.ParseForm()
	if err != nil {
		http.Error(rw, fmt.Sprintf("%v", err), http.StatusBadRequest)
		return
	}

	certRequestIDs, ok := req.Form["certRequestId"]
	if ok {
		certRequestID = certRequestIDs[0]
	}

	matched, _ := regexp.MatchString("^[A-Z2-7=]{16}$", certRequestID)
	if certRequestID != "" && !matched {
		http.Error(rw, "Invalid certRequestId", http.StatusBadRequest)
		return
	}
	log.Printf("List pending requests received from %s for request id '%s'\n",
		req.RemoteAddr, certRequestID)

	foundSomething := false
	results := make(map[string]listResponseElement)
	for k, v := range h.state {
		encodedCert := base64.StdEncoding.EncodeToString(v.request.Marshal())
		element := newResponseElement(v.environment, v.reason, encodedCert)
		// Two ways to use this URL. If caller specified a certRequestId
		// then we return only that one. Otherwise everything.
		if certRequestID == "" {
			results[k] = element
			foundSomething = true
		} else {
			if certRequestID == k {
				results[k] = element
				foundSomething = true
				break
			}
		}
	}
	if foundSomething {
		output, err := json.Marshal(results)
		if err != nil {
			http.Error(rw, fmt.Sprintf("Trouble marshaling json response %v", err), http.StatusInternalServerError)
			return
		}
		rw.Write(output)
	} else {
		http.Error(rw, fmt.Sprintf("No certs found."), http.StatusNotFound)
		return
	}
}

func (h *certRequestHandler) getRequestStatus(rw http.ResponseWriter, req *http.Request) {
	uriVars := mux.Vars(req)
	requestID := uriVars["requestID"]

	type Response struct {
		certSigned bool
		cert       string
	}
	if h.state[requestID].certSigned == true {
		rw.Write([]byte(h.state[requestID].request.Type()))
		rw.Write([]byte(" "))
		rw.Write([]byte(base64.StdEncoding.EncodeToString(h.state[requestID].request.Marshal())))
		rw.Write([]byte("\n"))
	} else {
		http.Error(rw, "Cert not signed yet.", http.StatusPreconditionFailed)
	}
}

func (h *certRequestHandler) signRequest(rw http.ResponseWriter, req *http.Request) {

	requestID := mux.Vars(req)["requestID"]
	originalRequest, ok := h.state[requestID]
	if !ok {
		http.Error(rw, "Unknown request id", http.StatusNotFound)
		return
	}

	err := req.ParseForm()
	if err != nil {
		http.Error(rw, fmt.Sprintf("%v", err), http.StatusBadRequest)
		return
	}

	envConfig, ok := h.Config[originalRequest.environment]
	if !ok {
		http.Error(rw, "Original request found to have an invalid env. Weird.", http.StatusBadRequest)
		return
	}

	signedCert, err := h.extractCertFromRequest(req, envConfig.AuthorizedSigners)
	if err != nil {
		log.Println("Invalid certificate signing request received, ignoring")
		http.Error(rw, fmt.Sprintf("%v", err), http.StatusBadRequest)
		return
	}

	signerFp := ssh_ca_util.MakeFingerprint(signedCert.SignatureKey.Marshal())

	// Verifying that the cert being posted to us here matches the one in the
	// request. That is, that an attacker isn't using an old signature to sign a
	// new/different request id
	requestedCert := h.state[requestID].request
	signedCert.SignatureKey = requestedCert.SignatureKey
	signedCert.Signature = nil
	requestedCert.Signature = nil
	// Resetting the Nonce felt wrong. But it turns out that when the signer
	// signs the request the act of signing generates a new Nonce. So it will
	// never match.
	requestedCert.Nonce = []byte("")
	signedCert.Nonce = []byte("")
	if !bytes.Equal(requestedCert.Marshal(), signedCert.Marshal()) {
		log.Println("Signature was valid, but cert didn't match.")
		log.Printf("Orig req: %#v\n", requestedCert)
		log.Printf("Sign req: %#v\n", signedCert)
		http.Error(rw, "Signature was valid, but cert didn't match.", http.StatusBadRequest)
		return
	}

	h.state[requestID].signatures[signerFp] = true
	log.Printf("Signature for serial %d id %s received from %s (%s) @ %s and determined valid\n",
		signedCert.Serial, requestID, signerFp, envConfig.AuthorizedSigners[signerFp], req.RemoteAddr)

	if len(h.state[requestID].signatures) >= envConfig.NumberSignersRequired {
		log.Printf("Received %d signatures for %s, signing now.\n", len(h.state[requestID].signatures), requestID)
		signer, err := ssh_ca_util.GetSignerForFingerprint(envConfig.SigningKeyFingerprint, h.sshAgentConn)
		if err != nil {
			log.Printf("Couldn't find signing key for request %s, unable to sign request\n", requestID)
			http.Error(rw, "Couldn't find signing key, unable to sign. Sorry.", http.StatusNotFound)
			return
		}
		stateInfo := h.state[requestID]
		stateInfo.request.SignCert(rand.Reader, signer)
		stateInfo.certSigned = true
		// this is weird. see: https://code.google.com/p/go/issues/detail?id=3117
		h.state[requestID] = stateInfo
	}

}

func signdFlags() []cli.Flag {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/"
	}
	configPath := home + "/.ssh_ca/sign_certd_config.json"

	return []cli.Flag{
		cli.StringFlag{
			Name:  "config-file",
			Value: configPath,
			Usage: "Path to config.json",
		},
	}
}

func signCertd(c *cli.Context) {
	configPath := c.String("config-file")
	config := make(map[string]ssh_ca_util.SignerdConfig)
	err := ssh_ca_util.LoadConfig(configPath, &config)
	if err != nil {
		log.Println("Load Config failed:", err)
		os.Exit(1)
	}
	runSignCertd(config)
}

func makeCertRequestHandler(config map[string]ssh_ca_util.SignerdConfig) certRequestHandler {
	var requestHandler certRequestHandler
	requestHandler.Config = config
	requestHandler.state = make(map[string]certRequest)
	requestHandler.NextSerial = make(chan uint64)
	go func() {
		var serial uint64
		for serial = 1; ; serial++ {
			requestHandler.NextSerial <- serial
		}
	}()
	return requestHandler
}

func runSignCertd(config map[string]ssh_ca_util.SignerdConfig) {
	log.Println("Server running version", ssh_ca_util.BuildVersion)
	log.Println("Server started with config", config)
	log.Println("Using SSH agent at", os.Getenv("SSH_AUTH_SOCK"))

	sshAgentConn, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK"))
	if err != nil {
		log.Println("Dial failed:", err)
		os.Exit(1)
	}
	requestHandler := makeCertRequestHandler(config)
	requestHandler.sshAgentConn = sshAgentConn

	r := mux.NewRouter()
	requests := r.Path("/cert/requests").Subrouter()
	requests.Methods("POST").HandlerFunc(requestHandler.createSigningRequest)
	requests.Methods("GET").HandlerFunc(requestHandler.listPendingRequests)
	request := r.Path("/cert/requests/{requestID}").Subrouter()
	request.Methods("GET").HandlerFunc(requestHandler.getRequestStatus)
	request.Methods("POST").HandlerFunc(requestHandler.signRequest)
	http.ListenAndServe(":8080", r)
}
