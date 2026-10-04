//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type browser struct{ client *http.Client }
type observed struct {
	status  int
	header  http.Header
	body    []byte
	cookies []*http.Cookie
}
type journey struct {
	ctx                   context.Context
	base, idp, sha, phase string
	admin                 *browser
	checks                []string
	observation           map[string]any
	evidence              []map[string]any
}
type login struct {
	browser               *browser
	state                 *http.Cookie
	callback, assertionID string
	form                  url.Values
	replayStartedAt       time.Time
	responseIssuedAt      time.Time
	assertionIssuedAt     time.Time
}
type counts struct {
	OIDCUsers    int `json:"oidc_users"`
	SAMLUsers    int `json:"saml_users"`
	OIDCSessions int `json:"oidc_sessions"`
	SAMLSessions int `json:"saml_sessions"`
	Assertions   int `json:"assertions"`
}

func newBrowser() *browser {
	jar, _ := cookiejar.New(nil)
	c := client()
	c.Jar = jar
	return &browser{client: c}
}
func (j *journey) fetch(b *browser, method, target string, body io.Reader, headers http.Header, extra *http.Cookie) (observed, error) {
	var result observed
	req, err := http.NewRequestWithContext(j.ctx, method, target, body)
	if err != nil {
		return result, err
	}
	if headers != nil {
		req.Header = headers.Clone()
	}
	if extra != nil {
		req.AddCookie(extra)
	}
	res, err := b.client.Do(req)
	if err != nil {
		return result, errors.New("fixture transport failed")
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 2*1024*1024+1))
	if err != nil {
		return result, errors.New("fixture response failed")
	}
	if len(data) > 2*1024*1024 {
		return result, errors.New("fixture response too large")
	}
	return observed{status: res.StatusCode, header: res.Header.Clone(), body: data, cookies: res.Cookies()}, nil
}
func require(ok bool, message string) error {
	if !ok {
		return errors.New(message)
	}
	return nil
}
func (j *journey) step(name string) { j.phase = name; j.observation = make(map[string]any) }
func (j *journey) pass() {
	j.checks = append(j.checks, j.phase)
	record := map[string]any{"name": j.phase}
	for key, value := range j.observation {
		record[key] = value
	}
	j.evidence = append(j.evidence, record)
	emit(map[string]any{"event": "check", "name": j.phase, "status": "pass", "evidence": record})
}
func (j *journey) begin(provider, returnTo, mode string) (login, error) {
	result := login{browser: newBrowser()}
	res, err := j.fetch(result.browser, "GET", j.base+"/auth/sso/"+provider+"/login?"+url.Values{"returnTo": {returnTo}}.Encode(), nil, nil, nil)
	if err != nil {
		return result, err
	}
	if err = require(res.status == 302, "login redirect missing"); err != nil {
		return result, err
	}
	cookieName := "caesium_" + provider + "_state"
	for _, cookie := range res.cookies {
		if cookie.Name == cookieName {
			copy := *cookie
			result.state = &copy
		}
	}
	if err = require(result.state != nil && result.state.HttpOnly && result.state.Path == "/auth/sso/"+provider && result.state.Value != "", "state cookie missing"); err != nil {
		return result, err
	}
	target, err := url.Parse(res.header.Get("Location"))
	if err != nil {
		return result, err
	}
	expected := j.idp + "/authorize"
	if provider == "saml" {
		expected = j.idp + "/saml/sso"
	}
	if err = require(target.Scheme+"://"+target.Host+target.Path == expected, "unexpected IdP redirect"); err != nil {
		return result, err
	}
	q := target.Query()
	if mode != "" {
		q.Set("fixture_mode", mode)
		target.RawQuery = q.Encode()
	}
	if provider == "oidc" {
		if err = require(q.Get("nonce") != "" && q.Get("state") != "" && q.Get("code_challenge_method") == "S256" && q.Get("code_challenge") != "", "PKCE authorization missing"); err != nil {
			return result, err
		}
	}
	result.replayStartedAt = time.Now()
	res, err = j.fetch(result.browser, "GET", target.String(), nil, nil, nil)
	if err != nil {
		return result, err
	}
	if provider == "oidc" {
		result.callback = res.header.Get("Location")
		if err = require(res.status == 302 && strings.HasPrefix(result.callback, j.base+"/auth/sso/oidc/callback?"), "authorization callback missing"); err != nil {
			return result, err
		}
	} else {
		if err = require(res.status == 200, "signed SAML form missing"); err != nil {
			return result, err
		}
		// The isolated IdP's fixed form is parsed; arbitrary third-party HTML is not.
		action := regexp.MustCompile(`<form method="post" action="([^"]+)"`).FindSubmatch(res.body)
		if len(action) != 2 || html.UnescapeString(string(action[1])) != j.base+"/auth/sso/saml/acs" {
			return result, errors.New("unexpected ACS form")
		}
		result.form = make(url.Values)
		for _, name := range []string{"SAMLResponse", "RelayState"} {
			match := regexp.MustCompile(`<input name="` + name + `" value="([^"]+)"`).FindSubmatch(res.body)
			if len(match) != 2 {
				return result, errors.New("SAML form value missing")
			}
			result.form.Set(name, html.UnescapeString(string(match[1])))
		}
		result.assertionID = res.header.Get("X-Fixture-Assertion-ID")
		result.responseIssuedAt, result.assertionIssuedAt, err = samlIssueInstants(result.form.Get("SAMLResponse"), result.assertionID)
		if err != nil {
			return result, err
		}
		if err = require(strings.HasPrefix(result.assertionID, "assertion-"), "assertion identity missing"); err != nil {
			return result, err
		}
	}
	return result, nil
}
func (j *journey) submit(l login, provider string, restore bool) (observed, error) {
	var cookie *http.Cookie
	if restore {
		cookie = l.state
	}
	var result observed
	var err error
	if provider == "oidc" {
		result, err = j.fetch(l.browser, "GET", l.callback, nil, nil, cookie)
	} else {
		result, err = j.fetch(l.browser, "POST", j.base+"/auth/sso/saml/acs", strings.NewReader(l.form.Encode()), http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, cookie)
	}
	if err == nil {
		j.observation["status_code"] = result.status
	}
	return result, err
}
func (j *journey) identity(b *browser) (map[string]any, error) {
	res, err := j.fetch(b, "GET", j.base+"/auth/whoami", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	if res.status != 200 {
		return nil, errors.New("session principal missing")
	}
	var value map[string]any
	if err = json.Unmarshal(res.body, &value); err != nil {
		return nil, err
	}
	if value["kind"] != "user" || value["role"] != "admin" || value["csrf_token"] == "" {
		return nil, errors.New("SSO role/CSRF mismatch")
	}
	return value, nil
}
func (j *journey) success(l login, provider, target string) (observed, error) {
	res, err := j.submit(l, provider, false)
	if err != nil {
		return res, err
	}
	if err = require(res.status == 302 && res.header.Get("Location") == target, "callback redirect mismatch"); err != nil {
		return res, err
	}
	cleared, session := false, false
	for _, c := range res.cookies {
		if c.Name == l.state.Name && c.MaxAge < 0 {
			cleared = true
		}
		if c.Name == "caesium_session" && c.Value != "" && c.HttpOnly {
			session = true
		}
	}
	if err = require(cleared && session, "callback cookie lifecycle mismatch"); err != nil {
		return res, err
	}
	who, err := j.identity(l.browser)
	if err != nil {
		return res, err
	}
	j.observation["redirect"] = res.header.Get("Location")
	j.observation["role"] = who["role"]
	expected := provider + "-coverage@example.invalid"
	if err = require(who["subject"] == expected, "SSO subject mismatch"); err != nil {
		return res, err
	}
	protected, err := j.fetch(l.browser, "GET", j.base+"/v1/jobs", nil, nil, nil)
	if err != nil {
		return res, err
	}
	if err = require(protected.status == 200, "authenticated protected read failed"); err != nil {
		return res, err
	}
	return res, nil
}
func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
func (j *journey) query(sql string) ([][]any, error) {
	who, err := j.identity(j.admin)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]any{"sql": sql, "limit": 100})
	res, err := j.fetch(j.admin, "POST", j.base+"/v1/database/query", bytes.NewReader(payload), http.Header{"Content-Type": {"application/json"}, "X-CSRF-Token": {who["csrf_token"].(string)}}, nil)
	if err != nil {
		return nil, err
	}
	if res.status != 200 {
		return nil, errors.New("authenticated DB read failed")
	}
	var data struct {
		Rows      [][]any `json:"rows"`
		RowCount  int     `json:"row_count"`
		Truncated bool    `json:"truncated"`
	}
	if err = json.Unmarshal(res.body, &data); err != nil {
		return nil, err
	}
	if data.Truncated || data.RowCount != len(data.Rows) {
		return nil, errors.New("incomplete DB result")
	}
	return data.Rows, nil
}
func (j *journey) counts() (counts, error) {
	var value counts
	rows, err := j.query("SELECT (SELECT COUNT(*) FROM users WHERE issuer = " + sqlQuote(j.idp) + "), (SELECT COUNT(*) FROM users WHERE issuer = " + sqlQuote(j.idp+"/saml/metadata") + "), (SELECT COUNT(*) FROM sessions WHERE auth_method = 'oidc'), (SELECT COUNT(*) FROM sessions WHERE auth_method = 'saml'), (SELECT COUNT(*) FROM saml_assertion_ids WHERE issuer = " + sqlQuote(j.idp+"/saml/metadata") + ")")
	if err != nil {
		return value, err
	}
	if len(rows) != 1 || len(rows[0]) != 5 {
		return value, errors.New("unexpected count projection")
	}
	values := []*int{&value.OIDCUsers, &value.SAMLUsers, &value.OIDCSessions, &value.SAMLSessions, &value.Assertions}
	for i, v := range rows[0] {
		n, ok := v.(float64)
		if !ok || n < 0 || n != float64(int(n)) {
			return value, errors.New("invalid count")
		}
		*values[i] = int(n)
	}
	j.observation["row_counts"] = value
	return value, nil
}
func (j *journey) unchanged(before counts) error {
	after, err := j.counts()
	if err != nil {
		return err
	}
	return require(after == before, "refused callback mutated identity/session/replay rows")
}
func (j *journey) oidcStats() (map[string]int, error) {
	res, err := j.fetch(newBrowser(), "GET", j.idp+"/stats", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	var stats map[string]int
	err = json.Unmarshal(res.body, &stats)
	return stats, err
}
func (j *journey) positive(provider, raw, target string) error {
	before, err := j.counts()
	if err != nil {
		return err
	}
	l, err := j.begin(provider, raw, "")
	if err != nil {
		return err
	}
	if _, err = j.success(l, provider, target); err != nil {
		return err
	}
	after, err := j.counts()
	if err != nil {
		return err
	}
	if provider == "oidc" {
		before.OIDCSessions++
		before.OIDCUsers = 1
	} else {
		before.SAMLSessions++
		before.SAMLUsers = 1
		before.Assertions++
	}
	return require(after == before, "successful callback accounting mismatch")
}
func (j *journey) oidc() error {
	j.step("oidc-success")
	first, err := j.begin("oidc", "/runs?status=mine#coverage", "")
	if err != nil {
		return err
	}
	if _, err = j.success(first, "oidc", "/runs?status=mine#coverage"); err != nil {
		return err
	}
	j.admin = first.browser
	before, err := j.counts()
	if err != nil {
		return err
	}
	if err = require(before == (counts{OIDCUsers: 1, OIDCSessions: 1}), "fixture database not fresh or first login accounting wrong"); err != nil {
		return err
	}
	j.pass()
	j.step("oidc-code-replay")
	stats, err := j.oidcStats()
	if err != nil {
		return err
	}
	res, err := j.submit(first, "oidc", true)
	if err != nil {
		return err
	}
	if err = require(res.status == 401, "code replay accepted"); err != nil {
		return err
	}
	if err = j.unchanged(before); err != nil {
		return err
	}
	afterStats, err := j.oidcStats()
	if err != nil {
		return err
	}
	if err = require(afterStats["rejected"] == stats["rejected"]+1, "replay did not reach one-use token exchange"); err != nil {
		return err
	}
	j.pass()
	for _, name := range []string{"state-mismatch", "missing-cookie", "tampered-cookie", "provider-error", "bad_nonce", "bad_audience"} {
		j.step("oidc-" + name)
		l, err := j.begin("oidc", "/", func() string {
			if name == "bad_nonce" || name == "bad_audience" {
				return name
			}
			return ""
		}())
		if err != nil {
			return err
		}
		before, err := j.counts()
		if err != nil {
			return err
		}
		stats, err := j.oidcStats()
		if err != nil {
			return err
		}
		switch name {
		case "state-mismatch":
			u, _ := url.Parse(l.callback)
			q := u.Query()
			q.Set("state", "wrong")
			u.RawQuery = q.Encode()
			l.callback = u.String()
		case "provider-error":
			l.callback = j.base + "/auth/sso/oidc/callback?error=access_denied&error_description=fixture"
		case "missing-cookie":
			l.browser = newBrowser()
		case "tampered-cookie":
			cookies := l.browser.client.Jar.Cookies(mustURL(j.base + "/auth/sso/oidc/callback"))
			for _, c := range cookies {
				if c.Name == l.state.Name {
					c.Value = tamperCookie(c.Value)
				}
			}
			l.browser.client.Jar.SetCookies(mustURL(j.base+"/auth/sso/oidc/callback"), cookies)
		}
		res, err := j.submit(l, "oidc", false)
		if err != nil {
			return err
		}
		if err = require(res.status == 401, "negative OIDC callback accepted"); err != nil {
			return err
		}
		if err = j.unchanged(before); err != nil {
			return err
		}
		current, err := j.oidcStats()
		if err != nil {
			return err
		}
		if name != "bad_nonce" && name != "bad_audience" {
			if err = require(current["redeemed"] == stats["redeemed"], "invalid state reached token exchange"); err != nil {
				return err
			}
		}
		j.pass()
	}
	j.step("oidc-fresh-positive")
	if err = j.positive("oidc", "/", "/"); err != nil {
		return err
	}
	j.pass()
	return nil
}
func tamperCookie(value string) string {
	payload, sig, ok := strings.Cut(value, ".")
	if !ok || sig == "" {
		return "invalid"
	}
	replacement := "A"
	if sig[0] == 'A' {
		replacement = "B"
	}
	return payload + "." + replacement + sig[1:]
}
func mustURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic("fixture URL invalid")
	}
	return u
}
func (j *journey) assertionCount(id string) (int, error) {
	rows, err := j.query("SELECT COUNT(*) FROM saml_assertion_ids WHERE issuer = " + sqlQuote(j.idp+"/saml/metadata") + " AND assertion_id = " + sqlQuote(id))
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return 0, errors.New("assertion count projection")
	}
	v, ok := rows[0][0].(float64)
	if !ok {
		return 0, errors.New("assertion count type")
	}
	return int(v), nil
}

// crewjam checks both Response and Assertion IssueInstant against its default
// 90-second MaxIssueDelay before invoking the replay store. Keep evidence well
// inside that interval; a 401 from an expired envelope proves no replay access.
const replayEvidenceMaxAge = 60 * time.Second

func samlIssueInstants(encoded, assertionID string) (time.Time, time.Time, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return time.Time{}, time.Time{}, errors.New("invalid SAML evidence envelope")
	}
	var envelope struct {
		XMLName      xml.Name  `xml:"urn:oasis:names:tc:SAML:2.0:protocol Response"`
		IssueInstant time.Time `xml:"IssueInstant,attr"`
		Assertion    struct {
			ID           string    `xml:"ID,attr"`
			IssueInstant time.Time `xml:"IssueInstant,attr"`
		} `xml:"urn:oasis:names:tc:SAML:2.0:assertion Assertion"`
	}
	if xml.Unmarshal(raw, &envelope) != nil || envelope.IssueInstant.IsZero() || envelope.Assertion.IssueInstant.IsZero() || envelope.Assertion.ID != assertionID {
		return time.Time{}, time.Time{}, errors.New("SAML evidence issue instant/identity missing")
	}
	return envelope.IssueInstant, envelope.Assertion.IssueInstant, nil
}

func requireFreshReplayEvidence(l login, now time.Time) error {
	for _, issued := range []time.Time{l.replayStartedAt, l.responseIssuedAt, l.assertionIssuedAt} {
		age := now.Sub(issued)
		if issued.IsZero() || age < 0 || age >= replayEvidenceMaxAge {
			return errors.New("SAML replay evidence exceeded strict issue-age bound")
		}
	}
	return nil
}

func (j *journey) replay(l login, before counts) error {
	if err := requireFreshReplayEvidence(l, time.Now()); err != nil {
		return err
	}
	res, err := j.submit(l, "saml", true)
	if err != nil {
		return err
	}
	if err = requireFreshReplayEvidence(l, time.Now()); err != nil {
		return err
	}
	if err = require(res.status == 401, "signed assertion replay accepted"); err != nil {
		return err
	}
	if err = j.unchanged(before); err != nil {
		return err
	}
	n, err := j.assertionCount(l.assertionID)
	if err != nil {
		return err
	}
	return require(n == 1, "exact accepted assertion row not retained")
}
func (j *journey) saml() error {
	j.step("saml-signed-success")
	before, err := j.counts()
	if err != nil {
		return err
	}
	l, err := j.begin("saml", "/runs?status=failed#coverage", "")
	if err != nil {
		return err
	}
	if _, err = j.success(l, "saml", "/runs?status=failed#coverage"); err != nil {
		return err
	}
	after, err := j.counts()
	if err != nil {
		return err
	}
	before.SAMLUsers = 1
	before.SAMLSessions++
	before.Assertions++
	if err = require(after == before, "SAML success accounting mismatch"); err != nil {
		return err
	}
	n, err := j.assertionCount(l.assertionID)
	if err != nil {
		return err
	}
	if err = require(n == 1, "signed assertion was not persisted"); err != nil {
		return err
	}
	j.pass()
	j.step("saml-immediate-replay")
	if err = j.replay(l, after); err != nil {
		return err
	}
	j.pass()
	j.step("saml-restart-barrier")
	digest := sha256.Sum256([]byte(l.form.Get("SAMLResponse")))
	emit(map[string]any{"event": "restart_required", "phase": "saml-persisted", "assertion_digest": hex.EncodeToString(digest[:])})
	barrier := make(chan error, 1)
	go func() {
		var ack struct {
			Event      string `json:"event"`
			Generation int    `json:"generation"`
		}
		decoder := json.NewDecoder(io.LimitReader(os.Stdin, 4096))
		decoder.DisallowUnknownFields()
		err := decoder.Decode(&ack)
		if err == nil && (ack.Event != "server_restarted" || ack.Generation != 2) {
			err = errors.New("invalid restart acknowledgment")
		}
		barrier <- err
	}()
	replayTimer := time.NewTimer(replayEvidenceMaxAge - time.Since(l.replayStartedAt))
	defer replayTimer.Stop()
	select {
	case <-replayTimer.C:
		return errors.New("restart exceeded SAML replay evidence issue-age bound")
	case <-j.ctx.Done():
		return errors.New("restart barrier deadline exceeded")
	case err := <-barrier:
		if err != nil {
			return errors.New("restart barrier refused")
		}
	}
	// The collector guarantees a new server process/image identity; the helper
	// independently observes readiness and persisted principal/replay state.
	if err = j.ready(); err != nil {
		return err
	}
	j.pass()
	j.step("saml-persistent-replay")
	if err = j.replay(l, after); err != nil {
		return err
	}
	j.pass()
	j.step("saml-fresh-positive")
	if err = j.positive("saml", "/", "/"); err != nil {
		return err
	}
	j.pass()
	for _, name := range []string{"tampered", "bad_audience", "expired", "relay-mismatch", "tampered-cookie", "missing-cookie", "missing-response"} {
		j.step("saml-" + name)
		mode := ""
		if name == "tampered" || name == "bad_audience" || name == "expired" {
			mode = name
		}
		negative, err := j.begin("saml", "/", mode)
		if err != nil {
			return err
		}
		before, err := j.counts()
		if err != nil {
			return err
		}
		switch name {
		case "relay-mismatch":
			negative.form.Set("RelayState", "wrong")
		case "missing-response":
			negative.form.Del("SAMLResponse")
		case "missing-cookie":
			negative.browser = newBrowser()
		case "tampered-cookie":
			cookies := negative.browser.client.Jar.Cookies(mustURL(j.base + "/auth/sso/saml/acs"))
			for _, cookie := range cookies {
				if cookie.Name == negative.state.Name {
					cookie.Value = tamperCookie(cookie.Value)
				}
			}
			negative.browser.client.Jar.SetCookies(mustURL(j.base+"/auth/sso/saml/acs"), cookies)
		}
		res, err := j.submit(negative, "saml", false)
		if err != nil {
			return err
		}
		if err = require(res.status == 401, "negative SAML callback accepted"); err != nil {
			return err
		}
		if err = j.unchanged(before); err != nil {
			return err
		}
		n, err := j.assertionCount(negative.assertionID)
		if err != nil {
			return err
		}
		if err = require(n == 0, "invalid assertion was recorded"); err != nil {
			return err
		}
		j.pass()
	}
	return nil
}
func (j *journey) ready() error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		res, err := j.fetch(newBrowser(), "GET", j.base+"/auth/status", nil, nil, nil)
		if err == nil && res.status == 200 {
			var body map[string]any
			if json.Unmarshal(res.body, &body) == nil && bytes.Contains(res.body, []byte("/auth/sso/oidc/login")) && bytes.Contains(res.body, []byte("/auth/sso/saml/login")) {
				return nil
			}
		}
		select {
		case <-j.ctx.Done():
			return errors.New("SSO readiness deadline exceeded")
		case <-ticker.C:
		}
	}
}
func runJourney(args []string) (err error) {
	fs := flag.NewFlagSet("journey", flag.ContinueOnError)
	base := fs.String("base", "", "task server origin")
	idp := fs.String("idp", "", "task IdP origin")
	sha := fs.String("candidate-sha", "", "full candidate SHA")
	timeout := fs.Duration("timeout", 4*time.Minute, "whole journey deadline")
	if err = fs.Parse(args); err != nil {
		return err
	}
	baseURL, err := origin(*base)
	if err != nil {
		return err
	}
	idpURL, err := origin(*idp)
	if err != nil {
		return err
	}
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(*sha) || *timeout <= 0 || *timeout > 4*time.Minute {
		return errors.New("invalid journey identity/deadline")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	j := &journey{observation: make(map[string]any), ctx: ctx, base: baseURL.String(), idp: idpURL.String(), sha: *sha, phase: "readiness"}
	defer func() {
		if err != nil {
			emit(map[string]any{"event": "failed", "phase": j.phase})
		}
	}()
	if err = j.ready(); err != nil {
		return err
	}
	j.pass()
	if err = j.oidc(); err != nil {
		return err
	}
	if err = j.saml(); err != nil {
		return err
	}
	for _, provider := range []string{"oidc", "saml"} {
		for i, item := range []struct{ raw, target string }{{j.base + "/runs?status=mine#absolute", "/runs?status=mine#absolute"}, {"/jobs%20escaped?x=%2F#frag%20ment", "/jobs%20escaped?x=%2F#frag%20ment"}, {"http://foreign.invalid/steal", "/"}, {"//foreign.invalid/steal", "/"}, {"relative", "/"}} {
			j.step(provider + "-return-target-" + strconv.Itoa(i))
			if err = j.positive(provider, item.raw, item.target); err != nil {
				return err
			}
			j.pass()
		}
	}
	final, err := j.counts()
	if err != nil {
		return err
	}
	emit(map[string]any{"event": "complete", "status": "pass", "candidate_sha": j.sha, "checks": j.checks, "evidence": j.evidence, "counts": final})
	return nil
}
