package log

import (
	"bufio"
	"bytes"
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
			zapcore.NewJSONEncoder(config()),
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

func TestLogTestSuite(t *testing.T) {
	suite.Run(t, new(LogTestSuite))
}
