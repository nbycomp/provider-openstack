package main

import (
	"io"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// newZapLogger builds the provider's logger. debug selects controller-runtime's
// development mode (debug level, no sampling) and format ("json" or "console")
// selects the encoder independently, so debug lines can be shipped as JSON.
func newZapLogger(debug bool, format string, w io.Writer) logr.Logger {
	// An explicit encoder bypasses controller-runtime's RFC3339 default; set it
	// so ts matches the operators writing to the same index.
	rfc3339 := func(ec *zapcore.EncoderConfig) { ec.EncodeTime = zapcore.RFC3339TimeEncoder }
	// Outside development mode controller-runtime defaults to JSON, so console
	// has to be chosen explicitly too.
	encoder := zap.JSONEncoder(rfc3339)
	if format == "console" {
		encoder = zap.ConsoleEncoder(rfc3339)
	}
	return zap.New(zap.UseDevMode(debug), zap.WriteTo(w), encoder)
}
