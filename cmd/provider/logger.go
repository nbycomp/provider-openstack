package main

import (
	"io"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// newZapLogger builds the provider's logger. debug sets only the level, so
// JSON output and debug lines can be combined; format is "json" or "console".
func newZapLogger(debug bool, format string, w io.Writer) logr.Logger {
	opts := []zap.Opts{zap.UseDevMode(debug), zap.WriteTo(w)}
	if format == "json" {
		// zap.JSONEncoder bypasses controller-runtime's RFC3339 default; set it
		// so ts matches the operators writing to the same index.
		opts = append(opts, zap.JSONEncoder(func(ec *zapcore.EncoderConfig) {
			ec.EncodeTime = zapcore.RFC3339TimeEncoder
		}))
	}
	return zap.New(opts...)
}
