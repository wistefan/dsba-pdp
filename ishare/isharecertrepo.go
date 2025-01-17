package ishare

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fiware/dsba-pdp/logging"
	"github.com/fiware/dsba-pdp/model"
	"github.com/procyon-projects/chrono"
)

const FingerprintsListEnvVar = "ISHARE_TRUSTED_FINGERPRINTS_LIST"
const SatellitUrlEnvVar = "ISHARE_TRUST_ANCHOR_URL"
const SatelliteIdEnvVar = "ISHARE_TRUST_ANCHOR_ID"
const SatelliteTokenPathEnvVar = "ISHARE_TRUST_ANCHOR_TOKEN_PATH"
const SatelliteTrustedListPathEnvVar = "ISHARE_TRUST_ANCHOR_TRUSTED_LIST_PATH"
const TrustedListUpdateRateEnvVar = "ISHARE_TRUSTED_LIST_UPDATE_RATE"

var satelliteURL = "https://scheme.isharetest.net"
var satelliteId = "EU.EORI.NL000000000"
var satelliteTokenPath = "/connect/token"
var satelliteTrustedListPath = "/trusted_list"
var satellitePartyPath = "/party/"
var updateRateInS = 5

type TrustedParticipantRepository interface {
	IsTrusted(caCertificate *x509.Certificate, clientCertificate *x509.Certificate, clientId string) (isTrusted bool)
}

type IShareTrustedParticipantRepository struct {
	satelliteAr           *model.AuthorizationRegistry
	trustedFingerprints   []string
	tokenFunc             TokenFunc
	trustedListParserFunc TrustedListParseFunc
	partyParseFunc        PartyParseFunc
}

func NewTrustedParticipantRepository(tokenFunc TokenFunc, trustedListParserFunc TrustedListParseFunc, partyParseFunc PartyParseFunc) *IShareTrustedParticipantRepository {

	trustedParticipantRepo := new(IShareTrustedParticipantRepository)

	fingerprintsString := os.Getenv(FingerprintsListEnvVar)
	if fingerprintsString == "" {
		logger.Fatal("No initial fingerprints configured for the satellite.")
		return nil
	}

	trustedParticipantRepo.trustedFingerprints = strings.Split(fingerprintsString, ",")

	logger.Debugf("Initially trusted fingerprints: %s.", trustedParticipantRepo.trustedFingerprints)

	satelliteUrlEnv := os.Getenv(SatellitUrlEnvVar)
	if satelliteUrlEnv != "" {
		satelliteURL = satelliteUrlEnv
	}
	satelliteIdEnv := os.Getenv(SatelliteIdEnvVar)
	if satelliteIdEnv != "" {
		satelliteId = satelliteIdEnv
	}
	satelliteTokenPathEnv := os.Getenv(SatelliteTokenPathEnvVar)
	if satelliteTokenPathEnv != "" {
		satelliteTokenPath = satelliteTokenPathEnv
	}
	satelliteTrustedListPathEnv := os.Getenv(SatelliteTrustedListPathEnvVar)
	if satelliteTrustedListPathEnv != "" {
		satelliteTrustedListPath = satelliteTrustedListPathEnv
	}

	updateRateInSEnv, err := strconv.Atoi(os.Getenv(TrustedListUpdateRateEnvVar))
	if err != nil {
		logger.Warnf("Invalid trustedlist update rate configured. Using the default %ds. Err: %s", updateRateInS, logging.PrettyPrintObject(err))
	} else if updateRateInSEnv > 0 {
		updateRateInS = updateRateInSEnv
	}
	ar := model.AuthorizationRegistry{Id: satelliteId, Host: satelliteURL, TokenPath: satelliteTokenPath}

	logger.Debugf("Using satellite %s as trust anchor.", logging.PrettyPrintObject(ar))
	trustedParticipantRepo.satelliteAr = &ar
	trustedParticipantRepo.tokenFunc = tokenFunc
	trustedParticipantRepo.trustedListParserFunc = trustedListParserFunc
	trustedParticipantRepo.partyParseFunc = partyParseFunc

	trustedParticipantRepo.scheduleTrustedListUpdate(updateRateInS)

	return trustedParticipantRepo
}

func (icr IShareTrustedParticipantRepository) scheduleTrustedListUpdate(updateRateInS int) {
	taskScheduler := chrono.NewDefaultTaskScheduler()
	taskScheduler.ScheduleAtFixedRate(icr.updateTrustedFingerprints, time.Duration(updateRateInS)*time.Second)
}

func (icr IShareTrustedParticipantRepository) IsTrusted(caCertificate *x509.Certificate, clientCertificate *x509.Certificate, clientId string) (isTrusted bool) {
	// check against trusted cas
	certificateFingerPrint := buildCertificateFingerprint(caCertificate)
	logger.Tracef("Checking certificate with fingerprint %s.", string(certificateFingerPrint))
	if contains(icr.trustedFingerprints, certificateFingerPrint) {
		logger.Tracef("The presented certificate is trusted.")
		return true
	}

	// ca is not listed, check just the party
	trustedParty, err := icr.getTrustedParty(clientId)
	if err != (model.HttpError{}) {
		logger.Infof("Was not able to get party info for %s. Err: %v", clientId, err)
		return false
	}
	for _, cert := range *trustedParty.Certificates {
		decodedCert, err := base64.StdEncoding.DecodeString(cert.X5c)
		if err != nil {
			logger.Warnf("The cert could not be decoded. Cert: %s", cert.X5c)
			return false
		}
		parsedCert, err := x509.ParseCertificate(decodedCert)
		if err != nil {
			logger.Warnf("The cert could not be parsed. Cert: %s", cert.X5c)
			return false
		}
		if buildCertificateFingerprint(parsedCert) == buildCertificateFingerprint(clientCertificate) {
			logger.Tracef("The presented certificate is listed for party %s.", clientId)
			return true
		}
	}
	return false
}

func (icr IShareTrustedParticipantRepository) updateTrustedFingerprints(ctx context.Context) {

	logger.Tracef("Certificate is not the satellite, request the current list.")
	trustedList, httpErr := icr.getTrustedList()
	if httpErr != (model.HttpError{}) {
		logger.Warnf("Was not able to get the trusted list. Err: %s", logging.PrettyPrintObject(httpErr))
		return
	}
	updatedFingerPrints := []string{}
	for _, trustedParticipant := range *trustedList {

		if trustedParticipant.Validity != "valid" {
			logger.Debugf("The participant %s is not valid.", logging.PrettyPrintObject(trustedParticipant))
			continue
		}
		if trustedParticipant.Status != "granted" {
			logger.Debugf("The participant %s is not granted.", logging.PrettyPrintObject(trustedParticipant))
			continue
		}
		updatedFingerPrints = append(updatedFingerPrints, trustedParticipant.CertificateFingerprint)
	}
	icr.trustedFingerprints = updatedFingerPrints
	logger.Tracef("Updated trusted fingerprints to: %s", icr.trustedFingerprints)
}

func (icr IShareTrustedParticipantRepository) getTrustedParty(id string) (trustedParty *model.PartyInfo, httpErr model.HttpError) {
	accessToken, httpErr := icr.tokenFunc(icr.satelliteAr)
	if httpErr != (model.HttpError{}) {
		logger.Debugf("Was not able to get a token from the satellite at %s.", logging.PrettyPrintObject(icr.satelliteAr))
		return trustedParty, httpErr
	}
	partyURL := icr.satelliteAr.Host + satellitePartyPath + id

	partyRequest, err := http.NewRequest("GET", partyURL, nil)
	if err != nil {
		logger.Debug("Was not able to create the trustedlist request.")
		return trustedParty, model.HttpError{Status: http.StatusInternalServerError, Message: "Was not able to create the request to the trusted list.", RootError: err}
	}
	partyRequest.Header.Set("Authorization", "Bearer "+accessToken)
	partyResponse, err := globalHttpClient.Do(partyRequest)
	if err != nil || partyResponse == nil {
		logger.Warnf("Was not able to get the trusted party %s from the satellite at %s.", id, logging.PrettyPrintObject(icr.satelliteAr))
		return trustedParty, model.HttpError{Status: http.StatusBadGateway, Message: "Was not able to retrieve the trusted list.", RootError: err}
	}
	if partyResponse.StatusCode != 200 {
		logger.Warnf("Was not able to get the trusted party %s. Status: %s, Message: %v", id, partyResponse.Status, partyResponse.Body)
		return trustedParty, model.HttpError{Status: http.StatusBadGateway, Message: "Was not able to retrieve the trusted party."}
	}

	var partyResponseObject model.TrustedPartyResponse
	err = json.NewDecoder(partyResponse.Body).Decode(&partyResponseObject)
	if err != nil {
		logger.Debugf("Was not able to decode the response body. Error: %v", err)
		return trustedParty, model.HttpError{Status: http.StatusBadGateway, Message: fmt.Sprintf("Received an invalid body from the satellite: %s", partyResponse.Body), RootError: err}
	}

	parsedToken, httpErr := icr.partyParseFunc(partyResponseObject.PartyToken)
	if httpErr != (model.HttpError{}) {
		logger.Debugf("Was not able to decode the ar response. Error: %v", httpErr)
		return trustedParty, httpErr
	}
	logger.Tracef("Trusted party response: %v", logging.PrettyPrintObject(parsedToken))
	return parsedToken.PartyInfo, httpErr
}

func (icr IShareTrustedParticipantRepository) getTrustedList() (trustedList *[]model.TrustedParticipant, httpErr model.HttpError) {
	accessToken, httpErr := icr.tokenFunc(icr.satelliteAr)
	if httpErr != (model.HttpError{}) {
		logger.Debugf("Was not able to get a token from the satellite at %s.", logging.PrettyPrintObject(icr.satelliteAr))
		return trustedList, httpErr
	}

	trustedListURL := icr.satelliteAr.Host + satelliteTrustedListPath

	trustedListRequest, err := http.NewRequest("GET", trustedListURL, nil)
	if err != nil {
		logger.Debug("Was not able to create the trustedlist request.")
		return trustedList, model.HttpError{Status: http.StatusInternalServerError, Message: "Was not able to create the request to the trusted list.", RootError: err}
	}
	trustedListRequest.Header.Set("Authorization", "Bearer "+accessToken)
	trustedListResponse, err := globalHttpClient.Do(trustedListRequest)
	if err != nil || trustedListResponse == nil {
		logger.Warnf("Was not able to get the trusted list from the satellite at %s.", logging.PrettyPrintObject(icr.satelliteAr))
		return trustedList, model.HttpError{Status: http.StatusBadGateway, Message: "Was not able to retrieve the trusted list.", RootError: err}
	}
	if trustedListResponse.StatusCode != 200 {
		logger.Warnf("Was not able to get a trusted list. Status: %s, Message: %v", trustedListResponse.Status, trustedListResponse.Body)
		return trustedList, model.HttpError{Status: http.StatusBadGateway, Message: "Was not able to retrieve the trusted list."}
	}

	var trustedListResponseObject model.TrustedListResponse
	err = json.NewDecoder(trustedListResponse.Body).Decode(&trustedListResponseObject)
	if err != nil {
		logger.Debugf("Was not able to decode the response body. Error: %v", err)
		return trustedList, model.HttpError{Status: http.StatusBadGateway, Message: fmt.Sprintf("Received an invalid body from the satellite: %s", trustedListResponse.Body), RootError: err}
	}
	parsedToken, httpErr := icr.trustedListParserFunc(trustedListResponseObject.TrustedListToken)
	if httpErr != (model.HttpError{}) {
		logger.Debugf("Was not able to decode the ar response. Error: %v", httpErr)
		return trustedList, httpErr
	}
	logger.Tracef("Trusted list response: %v", logging.PrettyPrintObject(parsedToken))
	return parsedToken.TrustedList, httpErr
}

func buildCertificateFingerprint(certificate *x509.Certificate) (fingerprint string) {

	fingerprintBytes := sha256.Sum256(certificate.Raw)

	var buf bytes.Buffer
	for i, f := range fingerprintBytes {
		if i > 0 {
			fmt.Fprintf(&buf, "")
		}
		fmt.Fprintf(&buf, "%02X", f)
	}

	return buf.String()
}

func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}
