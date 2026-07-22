// Package logx is a tiny leveled logger over the standard library.
// Keeping it stdlib-only avoids a dependency just for log levels.
package logx

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync/atomic"
)

type Level int32

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "?"
	}
}

// ParseLevel maps a string to a Level, defaulting to Info.
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

// Logger is safe for concurrent use. The level can be changed at runtime,
// which the web admin uses for hot reconfiguration.
type Logger struct {
	level int32 // atomic Level
	std   *log.Logger
}

var def = New(os.Stderr, LevelInfo)

// New builds a Logger writing to w.
func New(w io.Writer, lvl Level) *Logger {
	return &Logger{
		level: int32(lvl),
		std:   log.New(w, "", log.LstdFlags|log.Lmsgprefix),
	}
}

// Default returns the process-wide logger.
func Default() *Logger { return def }

// SetLevel changes the threshold at runtime.
func (l *Logger) SetLevel(lvl Level) { atomic.StoreInt32(&l.level, int32(lvl)) }

// GetLevel reports the logger's current level.
func (l *Logger) GetLevel() Level { return Level(atomic.LoadInt32(&l.level)) }

// SetOutput redirects the logger's writer at runtime. Used at startup to tee
// output to a log file (io.MultiWriter) in addition to the console.
func (l *Logger) SetOutput(w io.Writer) { l.std.SetOutput(w) }

// SetOutput redirects the process-wide logger's writer.
func SetOutput(w io.Writer) { def.SetOutput(w) }

// BestEffort wraps w so that write errors (and short writes) are swallowed
// and reported back as a full, successful write. This matters when w is
// chained inside an io.MultiWriter alongside another writer that must not be
// starved: io.MultiWriter.Write stops at the first writer that errors and
// never reaches the rest. A Windows service has no console at all, so
// os.Stderr/os.Stdout point at invalid handles and every write to them fails
// — without this wrapper, mirroring logs to "the console, plus a file" would
// silently lose the file copy too, purely because the console side failed
// first. BestEffort is for a writer whose failure is fine to ignore (the
// console mirror); the "real" sink (the log file) should stay unwrapped so
// its own errors still surface normally.
func BestEffort(w io.Writer) io.Writer { return bestEffortWriter{w} }

type bestEffortWriter struct{ w io.Writer }

func (b bestEffortWriter) Write(p []byte) (int, error) {
	b.w.Write(p) // intentionally ignore error/short-write — see BestEffort doc
	return len(p), nil
}

// Level reports the current threshold.
func (l *Logger) Level() Level { return Level(atomic.LoadInt32(&l.level)) }

func (l *Logger) log(lvl Level, format string, args ...any) {
	if lvl < l.Level() {
		return
	}
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	l.std.Printf("[%s] %s", lvl, msg)
}

func (l *Logger) Debugf(f string, a ...any) { l.log(LevelDebug, f, a...) }
func (l *Logger) Infof(f string, a ...any)  { l.log(LevelInfo, f, a...) }
func (l *Logger) Warnf(f string, a ...any)  { l.log(LevelWarn, f, a...) }
func (l *Logger) Errorf(f string, a ...any) { l.log(LevelError, f, a...) }

// Package-level shortcuts on the default logger.
func SetLevel(l Level) { def.SetLevel(l) }

// LevelName reports the process-global log level as a lowercase string, matching
// the values ParseLevel accepts and config.Config.LogLevel stores.
func LevelName() string { return strings.ToLower(def.GetLevel().String()) }

// CurrentLevel reports the process-global log level. Code deciding whether a
// configured level still needs applying should compare against this: the running
// logger is the only source of truth for what is actually in effect. Comparing
// against a config snapshot taken at startup silently breaks as soon as the
// level is changed at runtime, because the snapshot never moves — the level can
// then only ever be changed away from its boot value, once, and never back.
func CurrentLevel() Level { return def.GetLevel() }

func Debugf(f string, a ...any) { def.log(LevelDebug, f, a...) }
func Infof(f string, a ...any)  { def.log(LevelInfo, f, a...) }
func Warnf(f string, a ...any)  { def.log(LevelWarn, f, a...) }
func Errorf(f string, a ...any) { def.log(LevelError, f, a...) }
