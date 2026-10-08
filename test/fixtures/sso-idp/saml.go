//go:build integration

package main

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"html"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"time"

	crewsaml "github.com/crewjam/saml"
	dsig "github.com/russellhaering/goxmldsig"
)

type samlFixture struct {
	idp    *crewsaml.IdentityProvider
	spBase string
}

func newSAML(issuer, sp string, key *rsa.PrivateKey) (*samlFixture, error) {
	now := time.Now()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Caesium isolated SSO fixture"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	metadata, _ := url.Parse(issuer + "/saml/metadata")
	sso, _ := url.Parse(issuer + "/saml/sso")
	return &samlFixture{idp: &crewsaml.IdentityProvider{Signer: key, Certificate: cert, MetadataURL: *metadata, SSOURL: *sso, SignatureMethod: dsig.RSASHA256SignatureMethod}, spBase: sp}, nil
}
func (s *samlFixture) metadata() ([]byte, error) { return xml.Marshal(s.idp.Metadata()) }
func decodeAuthnRequest(encoded string) (crewsaml.AuthnRequest, error) {
	var req crewsaml.AuthnRequest
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return req, err
	}
	inflater := flate.NewReader(bytes.NewReader(raw))
	defer inflater.Close()
	data, err := io.ReadAll(io.LimitReader(inflater, 1024*1024+1))
	if err != nil {
		return req, err
	}
	if len(data) > 1024*1024 {
		return req, errors.New("AuthnRequest too large")
	}
	err = xml.Unmarshal(data, &req)
	return req, err
}
func (s *samlFixture) spMetadata(ctx context.Context) (*crewsaml.EntityDescriptor, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.spBase+"/auth/sso/saml/metadata", nil)
	if err != nil {
		return nil, err
	}
	res, err := client().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, errors.New("SP metadata refused")
	}
	return readSPMetadata(res.Body, s.spBase+"/auth/sso/saml/metadata")
}

func readSPMetadata(reader io.Reader, expectedEntity string) (*crewsaml.EntityDescriptor, error) {
	const maxMetadataBytes = 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(reader, maxMetadataBytes+1))
	if err != nil || len(body) > maxMetadataBytes {
		return nil, errors.New("SP metadata transport incomplete/oversized")
	}
	var metadata crewsaml.EntityDescriptor
	if err := xml.Unmarshal(body, &metadata); err != nil {
		return nil, err
	}
	if metadata.EntityID != expectedEntity {
		return nil, errors.New("unexpected SP entity")
	}
	return &metadata, nil
}

func (s *samlFixture) response(ctx context.Context, request crewsaml.AuthnRequest, relay, mode string) (crewsaml.IdpAuthnRequestForm, string, error) {
	var empty crewsaml.IdpAuthnRequestForm
	if request.ID == "" || request.Version != "2.0" || request.Issuer == nil || request.Issuer.Value != s.spBase+"/auth/sso/saml/metadata" || request.AssertionConsumerServiceURL != s.spBase+"/auth/sso/saml/acs" || request.Destination != s.idp.SSOURL.String() || request.ProtocolBinding != crewsaml.HTTPPostBinding || relay == "" {
		return empty, "", errors.New("invalid AuthnRequest")
	}
	metadata, err := s.spMetadata(ctx)
	if err != nil {
		return empty, "", err
	}
	if len(metadata.SPSSODescriptors) != 1 {
		return empty, "", errors.New("unexpected SP descriptors")
	}
	descriptor := &metadata.SPSSODescriptors[0]
	var endpoint *crewsaml.IndexedEndpoint
	for i := range descriptor.AssertionConsumerServices {
		e := &descriptor.AssertionConsumerServices[i]
		if e.Location == request.AssertionConsumerServiceURL && e.Binding == crewsaml.HTTPPostBinding {
			endpoint = e
		}
	}
	if endpoint == nil {
		return empty, "", errors.New("missing ACS endpoint")
	}
	now := time.Now().UTC()
	until := now.Add(5 * time.Minute)
	assertionID := "assertion-" + randomID()
	audience := metadata.EntityID
	if mode == "expired" {
		until = now.Add(-crewsaml.MaxClockSkew - time.Minute)
	}
	if mode == "bad_audience" {
		audience = "http://unrelated.invalid/metadata"
	}
	assertion := &crewsaml.Assertion{ID: assertionID, IssueInstant: now, Version: "2.0", Issuer: crewsaml.Issuer{Format: "urn:oasis:names:tc:SAML:2.0:nameid-format:entity", Value: s.idp.MetadataURL.String()},
		Subject:             &crewsaml.Subject{NameID: &crewsaml.NameID{Format: string(crewsaml.PersistentNameIDFormat), NameQualifier: s.idp.MetadataURL.String(), SPNameQualifier: metadata.EntityID, Value: "coverage-saml-subject"}, SubjectConfirmations: []crewsaml.SubjectConfirmation{{Method: "urn:oasis:names:tc:SAML:2.0:cm:bearer", SubjectConfirmationData: &crewsaml.SubjectConfirmationData{InResponseTo: request.ID, NotOnOrAfter: until, Recipient: request.AssertionConsumerServiceURL}}}},
		Conditions:          &crewsaml.Conditions{NotBefore: now.Add(-time.Minute), NotOnOrAfter: until, AudienceRestrictions: []crewsaml.AudienceRestriction{{Audience: crewsaml.Audience{Value: audience}}}},
		AuthnStatements:     []crewsaml.AuthnStatement{{AuthnInstant: now.Add(-time.Minute), SessionIndex: "session-" + randomID(), AuthnContext: crewsaml.AuthnContext{AuthnContextClassRef: &crewsaml.AuthnContextClassRef{Value: "urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport"}}}},
		AttributeStatements: []crewsaml.AttributeStatement{{Attributes: []crewsaml.Attribute{{Name: "email", Values: []crewsaml.AttributeValue{{Type: "xs:string", Value: "saml-coverage@example.invalid"}}}, {FriendlyName: "displayName", Values: []crewsaml.AttributeValue{{Type: "xs:string", Value: "Coverage SAML"}}}, {Name: "groups", Values: []crewsaml.AttributeValue{{Type: "xs:string", Value: "coverage-admins"}, {Type: "xs:string", Value: "coverage-readers"}}}}}}}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, s.idp.SSOURL.String(), nil)
	if err != nil {
		return empty, "", err
	}
	response := &crewsaml.IdpAuthnRequest{Now: now, IDP: s.idp, RelayState: relay, HTTPRequest: httpReq, Request: request, ServiceProviderMetadata: metadata, SPSSODescriptor: descriptor, ACSEndpoint: endpoint, Assertion: assertion}
	if err = response.MakeResponse(); err != nil {
		return empty, "", err
	}
	form, err := response.PostBinding()
	if err != nil {
		return empty, "", err
	}
	if mode == "tampered" {
		raw, err := base64.StdEncoding.DecodeString(form.SAMLResponse)
		if err != nil {
			return empty, "", err
		}
		if !bytes.Contains(raw, []byte("saml-coverage@example.invalid")) {
			return empty, "", errors.New("tamper target missing")
		}
		form.SAMLResponse = base64.StdEncoding.EncodeToString(bytes.Replace(raw, []byte("saml-coverage@example.invalid"), []byte("tampered-coverage@example.invalid"), 1))
	}
	return form, assertionID, nil
}
func (s *samlFixture) sso(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode := q.Get("fixture_mode")
	if r.Method != "GET" || !modeAllowed(mode, "expired", "bad_audience", "tampered") {
		http.Error(w, "invalid SAML fixture request", 400)
		return
	}
	request, err := decodeAuthnRequest(q.Get("SAMLRequest"))
	if err != nil {
		http.Error(w, "invalid AuthnRequest", 400)
		return
	}
	form, id, err := s.response(r.Context(), request, q.Get("RelayState"), mode)
	if err != nil {
		http.Error(w, "SAML fixture response failed", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Fixture-Assertion-ID", id)
	_, _ = io.WriteString(w, `<html><body><form method="post" action="`+html.EscapeString(form.URL)+`"><input name="SAMLResponse" value="`+html.EscapeString(form.SAMLResponse)+`"><input name="RelayState" value="`+html.EscapeString(form.RelayState)+`"></form></body></html>`)
}
