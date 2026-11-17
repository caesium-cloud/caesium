package reproduce

import (
	"strings"
	"testing"
)

func TestRedactHashInputBlobHidesUppercaseCredentialNames(t *testing.T) {
	const token = "review-only-credential"
	const password = "plaintext-secret"
	const apiKey = "uppercase-api-key"
	const authorization = "uppercase-authorization"
	raw := []byte(`{
		"runParams": {"API_TOKEN": "` + token + `", "API_KEY": "` + apiKey + `", "flavor": "vanilla"},
		"predecessorOutputs": {"extract": {"PASSWORD": "` + password + `", "AUTHORIZATION": "` + authorization + `"}}
	}`)

	out, err := redactHashInputBlob(raw)
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	for _, secret := range []string{token, password, apiKey, authorization} {
		if strings.Contains(body, secret) {
			t.Fatalf("hash input blob leaked %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, "vanilla") {
		t.Fatalf("non-credential param was redacted: %s", body)
	}
	for _, field := range []string{
		"runParams.PASSWORD",
		"runParams.API_TOKEN",
		"runParams.API_KEY",
		"predecessorOutputs.extract.AUTHORIZATION",
		"env.dbPassword",
	} {
		if !sensitiveField(field) {
			t.Errorf("%s should be treated as a credential name", field)
		}
	}
}
