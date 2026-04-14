package http

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"hash"
	stdhttp "net/http"
	"strings"
)

const (
	signatureSchemeHMACSHA256 = "hmac-sha256"
	signatureSchemeHMACSHA1   = "hmac-sha1"
	signatureSchemeBearer     = "bearer"
	signatureSchemeBasic      = "basic"
)

func validateSignature(req *stdhttp.Request, body []byte, secret, scheme, header, signedTimestamp string) bool {
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	secret = strings.TrimSpace(secret)

	if secret == "" {
		return true
	}

	if scheme == "" {
		scheme = signatureSchemeHMACSHA256
	}

	header = strings.TrimSpace(header)
	if header == "" {
		header = defaultSignatureHeader(scheme)
	}

	switch scheme {
	case signatureSchemeHMACSHA256:
		return validateHMAC(req.Header.Get(header), body, secret, sha256.New, "sha256", signedTimestamp)
	case signatureSchemeHMACSHA1:
		return validateHMAC(req.Header.Get(header), body, secret, sha1.New, "sha1", signedTimestamp)
	case signatureSchemeBearer:
		return validateBearer(req.Header.Get(header), secret)
	case signatureSchemeBasic:
		return validateBasic(req, secret)
	default:
		return false
	}
}

func defaultSignatureHeader(scheme string) string {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case signatureSchemeHMACSHA1:
		return "X-Hub-Signature"
	case signatureSchemeBearer, signatureSchemeBasic:
		return "Authorization"
	default:
		return "X-Hub-Signature-256"
	}
}

func validateHMAC(signature string, body []byte, secret string, hash func() hash.Hash, prefix, timestamp string) bool {
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return false
	}

	mac := hmac.New(hash, []byte(secret))
	if timestamp != "" {
		_, _ = mac.Write([]byte(timestamp + "."))
	}
	_, _ = mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	candidate := signature
	if prefix != "" && strings.HasPrefix(strings.ToLower(candidate), prefix+"=") {
		candidate = strings.TrimSpace(candidate[len(prefix)+1:])
	}

	return subtle.ConstantTimeCompare([]byte(strings.ToLower(candidate)), []byte(expected)) == 1
}

func validateBearer(headerValue, secret string) bool {
	headerValue = strings.TrimSpace(headerValue)
	if headerValue == "" {
		return false
	}

	const prefix = "bearer "
	if len(headerValue) < len(prefix) || !strings.EqualFold(headerValue[:len(prefix)], prefix) {
		return false
	}

	token := strings.TrimSpace(headerValue[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1
}

func validateBasic(req *stdhttp.Request, secret string) bool {
	user, pass, ok := req.BasicAuth()
	if !ok {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(user+":"+pass), []byte(secret)) == 1
}
