package main

import (
	"io"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// newZapLogger builds the provider's logger. debug selects controller-runtime's
// development mode (debug level, no sampling) and format ("json" or "console")
// selects the encoder independently, so debug lines can be shipped as JSON.
func newZapLogger(debug bool, format string, w io.Writer) logr.Logger {
	// An explicit encoder bypasses controller-runtime's RFC3339 default. Set it,
	// and string durations ("30s"), so ts and durations match the operators
	// writing to the same index.
	encoderConfig := func(ec *zapcore.EncoderConfig) {
		ec.EncodeTime = zapcore.RFC3339TimeEncoder
		ec.EncodeDuration = zapcore.StringDurationEncoder
	}
	// Outside development mode controller-runtime defaults to JSON, so console
	// has to be chosen explicitly too.
	encoder := zap.JSONEncoder(encoderConfig)
	if format == "console" {
		encoder = zap.ConsoleEncoder(encoderConfig)
	}
	return zap.New(zap.UseDevMode(debug), zap.WriteTo(w), encoder)
}

// logFormatFlag reads LOG_FORMAT too, so a deployment whose args are fixed can
// still switch to console through its env.
func logFormatFlag(app *kingpin.Application) *string {
	return app.Flag("log-format", "Log output format: json or console.").Default("json").Envar("LOG_FORMAT").Enum("json", "console")
}
