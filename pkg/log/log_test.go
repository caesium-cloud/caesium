package log

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type LogTestSuite struct {
	suite.Suite
}

func (s *LogTestSuite) TestLog() {
	assert.Nil(s.T(), SetLevel("debug"))
	assert.Equal(s.T(), zapcore.DebugLevel, GetLevel())
	assert.NotEmpty(s.T(), capture(Debug, "debug msg", "key", "value"))
	assert.NotEmpty(s.T(), capture(Info, "info msg", "key", "value"))
	assert.NotEmpty(s.T(), capture(Warn, "warn msg", "key", "value"))
	assert.NotEmpty(s.T(), capture(Error, "error msg", "key", "value"))
	assert.Panics(s.T(), func() { Panic("panic msg", "key", "value") })

	assert.Nil(s.T(), SetLevel("info"))
	assert.Equal(s.T(), zapcore.InfoLevel, GetLevel())
	assert.Empty(s.T(), capture(Debug, "debug msg", "key", "value"))
	assert.NotEmpty(s.T(), capture(Info, "info msg", "key", "value"))
	assert.NotEmpty(s.T(), capture(Warn, "warn msg", "key", "value"))
	assert.NotEmpty(s.T(), capture(Error, "error msg", "key", "value"))
	assert.Panics(s.T(), func() { Panic("panic msg", "key", "value") })

	assert.Nil(s.T(), SetLevel("warn"))
	assert.Equal(s.T(), zapcore.WarnLevel, GetLevel())
	assert.Empty(s.T(), capture(Debug, "debug msg", "key", "value"))
	assert.Empty(s.T(), capture(Info, "info msg", "key", "value"))
	assert.NotEmpty(s.T(), capture(Warn, "warn msg", "key", "value"))
	assert.NotEmpty(s.T(), capture(Error, "error msg", "key", "value"))
	assert.Panics(s.T(), func() { Panic("panic msg", "key", "value") })

	assert.Nil(s.T(), SetLevel("error"))
	assert.Equal(s.T(), zapcore.ErrorLevel, GetLevel())
	assert.Empty(s.T(), capture(Debug, "debug msg", "key", "value"))
	assert.Empty(s.T(), capture(Info, "info msg", "key", "value"))
	assert.Empty(s.T(), capture(Warn, "warn msg", "key", "value"))
	assert.NotEmpty(s.T(), capture(Error, "error msg", "key", "value"))
	assert.Panics(s.T(), func() { Panic("panic msg", "key", "value") })

	assert.Nil(s.T(), SetLevel("panic"))
	assert.Equal(s.T(), zapcore.PanicLevel, GetLevel())
	assert.Empty(s.T(), capture(Debug, "debug msg", "key", "value"))
	assert.Empty(s.T(), capture(Info, "info msg", "key", "value"))
	assert.Empty(s.T(), capture(Warn, "warn msg", "key", "value"))
	assert.Empty(s.T(), capture(Error, "error msg", "key", "value"))
	assert.Panics(s.T(), func() { Panic("panic msg", "key", "value") })

	assert.Nil(s.T(), SetLevel("fatal"))
	assert.Equal(s.T(), zapcore.FatalLevel, GetLevel())

	assert.NotNil(s.T(), SetLevel("bogus"))

	assert.Equal(s.T(), Clean("Hello World\n"), "hello world")
}

func capture(logFunc func(string, ...interface{}), msg string, kv ...interface{}) string {
	var buffer bytes.Buffer

	oldLogger := zap.S()

	writer := bufio.NewWriter(&buffer)

	zap.ReplaceGlobals(zap.New(
		zapcore.NewCore(
			zapcore.NewJSONEncoder(jsonConfig()),
			zapcore.AddSync(writer),
			logLevel,
		),
	))

	logFunc(msg, kv...)
	if err := writer.Flush(); err != nil {
		panic(err)
	}

	zap.ReplaceGlobals(oldLogger.Desugar())

	return buffer.String()
}

// captureFD redirects the given *os.File (os.Stdout or os.Stderr) to a pipe for
// the duration of fn and returns whatever was written to it. The logger must be
// (re)built INSIDE fn so it locks onto the swapped fd.
func captureFD(fd **os.File, fn func()) string {
	orig := *fd
	r, w, err := os.Pipe()
	if err != nil || r == nil || w == nil {
		// Don't assign a nil *os.File to the global fd — a later write would
		// nil-panic in unrelated code. Run fn against the original fd instead.
		fn()
		return ""
	}
	*fd = w
	fn()
	_ = w.Close()
	*fd = orig
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return buf.String()
}

// TestOutputRoutingKeepsStdoutClean is the regression for the CLI log
// contamination bug: a log line written under the default (stderr) routing must
// NOT land on stdout, so machine-readable command output (e.g. a
// `caesium receipt get` JSON receipt) stays parseable. ToStdout restores the
// server's stdout routing.
func (s *LogTestSuite) TestOutputRoutingKeepsStdoutClean() {
	s.Require().NoError(SetLevel("info"))
	defer ToStderr() // restore the package default for other tests

	// Default routing (stderr): the line appears on stderr, never on stdout.
	var onStderr string
	onStdout := captureFD(&os.Stdout, func() {
		onStderr = captureFD(&os.Stderr, func() {
			ToStderr()
			Info("routing probe alpha")
		})
	})
	s.Contains(onStderr, "routing probe alpha", "log should be on stderr")
	s.NotContains(onStdout, "routing probe alpha", "stdout must stay clean for machine output")

	// ToStdout routing (server): the line appears on stdout.
	onStdout2 := captureFD(&os.Stdout, func() {
		ToStdout()
		Info("routing probe beta")
	})
	s.Contains(onStdout2, "routing probe beta", "ToStdout should route logs to stdout")
}

func TestLogTestSuite(t *testing.T) {
	suite.Run(t, new(LogTestSuite))
}
