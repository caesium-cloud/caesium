package auditlog

import (
	"errors"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestLogFailurePreservesWarningAndOriginalError(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	restore := zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(restore)
	LogFailure(nil)
	if logs.Len() != 0 {
		t.Fatal("nil error emitted a warning")
	}
	failure := errors.New("audit unavailable")
	LogFailure(failure)
	entries := logs.All()
	if len(entries) != 1 || entries[0].Message != "failed to write audit log" || entries[0].Level != zapcore.WarnLevel {
		t.Fatalf("warning = %v", entries)
	}
	if len(entries[0].Context) != 1 || entries[0].Context[0].Key != "error" || entries[0].Context[0].Interface != failure {
		t.Fatalf("warning field = %v", entries[0].Context)
	}
}
