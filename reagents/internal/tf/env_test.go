package tf

import (
	"os"
	"testing"
)

func TestEnvironmentWithCopiesValuesAndOverrides(t *testing.T) {
	t.Setenv("REAGENTS_ENV_COPY", "a=b=c")
	t.Setenv("TF_DATA_DIR", "original")
	got := EnvironmentWith("TF_DATA_DIR", "override")
	if got["REAGENTS_ENV_COPY"] != "a=b=c" || got["TF_DATA_DIR"] != "override" {
		t.Fatal("copy lost equals signs or override")
	}
	got["REAGENTS_ENV_COPY"] = "changed"
	if os.Getenv("REAGENTS_ENV_COPY") != "a=b=c" || os.Getenv("TF_DATA_DIR") != "original" {
		t.Fatal("copy mutated process environment")
	}
}
