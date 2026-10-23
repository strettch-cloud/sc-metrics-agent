package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap/zapcore"
)

// Each name config accepts sets the lowest level the logger writes; an
// unknown one, which config never passes on, falls back to info.
func TestInitLoggerWritesFromTheLevelItIsGiven(t *testing.T) {
	for name, lowest := range map[string]zapcore.Level{
		"debug": zapcore.DebugLevel, "info": zapcore.InfoLevel, "warn": zapcore.WarnLevel,
		"error": zapcore.ErrorLevel, "fatal": zapcore.FatalLevel, "panic": zapcore.PanicLevel,
		"verbose": zapcore.InfoLevel,
	} {
		core := initLogger(name).Core()

		assert.True(t, core.Enabled(lowest), "%s: %s isn't written", name, lowest)
		assert.False(t, core.Enabled(lowest-1), "%s: %s is written", name, lowest-1)
	}
}
