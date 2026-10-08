package tf

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewTerraformRoutesBothChildStreamsThroughSerializedWriter(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "terraform")
	script := `#!/bin/sh
if [ "$1" = "version" ]; then
 echo '{"terraform_version":"1.15.9","platform":"linux_amd64","provider_selections":{},"terraform_outdated":false}'
 exit 0
fi
(i=0; while [ "$i" -lt 300 ]; do echo child-stdout; i=$((i+1)); done) &
(i=0; while [ "$i" -lt 300 ]; do echo child-stderr >&2; i=$((i+1)); done) &
wait
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer // Deliberately requires serialization under -race.
	terraform, writer, err := NewTerraform(dir, executable, &log)
	if err != nil {
		t.Fatal(err)
	}
	if err := terraform.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("caller-log\n")); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"child-stdout\n", "child-stderr\n"} {
		if got := strings.Count(log.String(), marker); got != 300 {
			t.Fatalf("%q count = %d", marker, got)
		}
	}
	if !strings.HasSuffix(log.String(), "caller-log\n") {
		t.Fatal("returned writer did not share the log")
	}
}

func TestNewTerraformConstructorFailure(t *testing.T) {
	terraform, writer, err := NewTerraform(t.TempDir(), "", &bytes.Buffer{})
	if err == nil || terraform != nil || writer != nil {
		t.Fatalf("constructor = %v, %v, %v", terraform, writer, err)
	}
}
