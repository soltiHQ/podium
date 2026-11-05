package raft

import (
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	hraft "github.com/hashicorp/raft"
	"github.com/rs/zerolog"
)

// Compile-time check.
var _ hclog.Logger = (*hclogAdapter)(nil)

// redacted stands for a value that must not reach a log.
const redacted = "[redacted]"

// hclogAdapter is the logger hashicorp/raft, its snapshot store and its network
// transport log through, in place of the hclog loggers they would make on
// stderr. It writes into the node's logger, in these levels:
//
//   - INFO, WARN and ERROR keep their level; hashicorp's own level for a line
//     is not changed.
//   - TRACE and DEBUG lines are dropped: INFO is the floor, and SetLevel does not
//     move it. Raft's elections and its replication chatter log at DEBUG.
//
// Each key and value pair of a line becomes a field of the event, and the name
// of the logger (raft, snapshot, raft-net) the field "component". A key that
// zerolog or the adapter writes itself, the time, the level, the message and the
// component, gets the prefix raft_ (raft logs the duration of a lost contact as
// "time"): the event never has the same key twice. A value is logged as its
// text; a byte slice, which could be what a command carries, only as its length,
// and the value of "command" never, except a change of configuration: raft
// prints a request it did not expect whole under that key, a log entry's data
// included, and the kind of a change of the configuration under it as well.
type hclogAdapter struct {
	logger  zerolog.Logger
	name    string
	implied []any // key and value pairs of With
}

// newHCLog returns the hclog.Logger of the hashicorp/raft component name, which
// logs into logger.
func newHCLog(logger zerolog.Logger, name string) hclog.Logger {
	return &hclogAdapter{logger: logger, name: name}
}

// Log implements hclog.Logger.
func (a *hclogAdapter) Log(level hclog.Level, msg string, args ...any) {
	var event *zerolog.Event
	switch level {
	case hclog.NoLevel, hclog.Info:
		event = a.logger.Info()
	case hclog.Warn:
		event = a.logger.Warn()
	case hclog.Error:
		event = a.logger.Error()
	}
	if !event.Enabled() {
		return
	}
	if a.name != "" {
		event = event.Str(componentKey, a.name)
	}
	event = addFields(event, a.implied)
	addFields(event, args).Msg(msg)
}

// addFields adds the key and value pairs of args to event. A key that is no
// string is written as one, and a key without a value gets the value hclog
// gives it.
func addFields(event *zerolog.Event, args []any) *zerolog.Event {
	for i := 0; i < len(args); i += 2 {
		key := fieldKey(args[i])
		if i+1 == len(args) {
			event = event.Str(key, "EXTRA_VALUE_AT_END")
			break
		}
		event = addField(event, key, args[i+1])
	}
	return event
}

// fieldKey is the name that the key of a pair has in the event: the key, with
// the prefix raft_ when it is a name that zerolog or the adapter writes on every
// event, which an event would carry twice.
func fieldKey(key any) string {
	name := fmt.Sprint(key)
	switch name {
	case zerolog.TimestampFieldName, zerolog.LevelFieldName, zerolog.MessageFieldName, componentKey:
		return "raft_" + name
	}
	return name
}

// componentKey is the field that names the component that logged a line.
const componentKey = "component"

// addField adds one field to event, as text unless the value is a number or a
// boolean.
func addField(event *zerolog.Event, key string, value any) *zerolog.Event {
	if key == "command" {
		// A change of the configuration shows its kind, such as AddVoter, with the ID and the
		// address of the server it changes in the pairs next to it. Any other value is redacted.
		if change, ok := value.(hraft.ConfigurationChangeCommand); ok {
			return event.Str(key, change.String())
		}
		return event.Str(key, redacted)
	}
	switch v := value.(type) {
	case nil:
		return event.Interface(key, nil)
	case string:
		return event.Str(key, v)
	case bool:
		return event.Bool(key, v)
	case int:
		return event.Int(key, v)
	case int8:
		return event.Int64(key, int64(v))
	case int16:
		return event.Int64(key, int64(v))
	case int32:
		return event.Int64(key, int64(v))
	case int64:
		return event.Int64(key, v)
	case uint:
		return event.Uint64(key, uint64(v))
	case uint8:
		return event.Uint64(key, uint64(v))
	case uint16:
		return event.Uint64(key, uint64(v))
	case uint32:
		return event.Uint64(key, uint64(v))
	case uint64:
		return event.Uint64(key, v)
	case float32:
		return event.Float64(key, float64(v))
	case float64:
		return event.Float64(key, v)
	case time.Time:
		return event.Time(key, v)
	case []byte:
		return event.Str(key, fmt.Sprintf("[%d bytes]", len(v)))
	case error:
		return event.Str(key, v.Error())
	case hclog.Format:
		return event.Str(key, formatted(v))
	case fmt.Stringer:
		return event.Str(key, v.String())
	default:
		return event.Str(key, fmt.Sprint(v))
	}
}

// formatted renders what hclog.Fmt wrapped: a format and its arguments.
func formatted(f hclog.Format) string {
	if len(f) > 0 {
		if format, ok := f[0].(string); ok {
			return fmt.Sprintf(format, f[1:]...)
		}
	}
	return fmt.Sprint([]any(f)...)
}

// Trace implements hclog.Logger. The adapter logs nothing below INFO.
func (a *hclogAdapter) Trace(string, ...any) {}

// Debug implements hclog.Logger. The adapter logs nothing below INFO.
func (a *hclogAdapter) Debug(string, ...any) {}

// Info implements hclog.Logger.
func (a *hclogAdapter) Info(msg string, args ...any) { a.Log(hclog.Info, msg, args...) }

// Warn implements hclog.Logger.
func (a *hclogAdapter) Warn(msg string, args ...any) { a.Log(hclog.Warn, msg, args...) }

// Error implements hclog.Logger.
func (a *hclogAdapter) Error(msg string, args ...any) { a.Log(hclog.Error, msg, args...) }

// IsTrace implements hclog.Logger.
func (a *hclogAdapter) IsTrace() bool { return false }

// IsDebug implements hclog.Logger.
func (a *hclogAdapter) IsDebug() bool { return false }

// IsInfo implements hclog.Logger.
func (a *hclogAdapter) IsInfo() bool { return a.logger.GetLevel() <= zerolog.InfoLevel }

// IsWarn implements hclog.Logger.
func (a *hclogAdapter) IsWarn() bool { return a.logger.GetLevel() <= zerolog.WarnLevel }

// IsError implements hclog.Logger.
func (a *hclogAdapter) IsError() bool { return a.logger.GetLevel() <= zerolog.ErrorLevel }

// ImpliedArgs implements hclog.Logger.
func (a *hclogAdapter) ImpliedArgs() []any { return append([]any(nil), a.implied...) }

// With implements hclog.Logger: the logger it returns adds args to every line.
func (a *hclogAdapter) With(args ...any) hclog.Logger {
	sub := *a
	sub.implied = append(append([]any(nil), a.implied...), args...)
	return &sub
}

// Name implements hclog.Logger.
func (a *hclogAdapter) Name() string { return a.name }

// Named implements hclog.Logger: the name it returns is the name of a, a dot and
// name.
func (a *hclogAdapter) Named(name string) hclog.Logger {
	sub := *a
	if a.name != "" {
		name = a.name + "." + name
	}
	sub.name = name
	return &sub
}

// ResetNamed implements hclog.Logger.
func (a *hclogAdapter) ResetNamed(name string) hclog.Logger {
	sub := *a
	sub.name = name
	return &sub
}

// SetLevel implements hclog.Logger. It does nothing: the floor stays at INFO.
func (a *hclogAdapter) SetLevel(hclog.Level) {}

// GetLevel implements hclog.Logger.
func (a *hclogAdapter) GetLevel() hclog.Level { return hclog.Info }

// StandardLogger implements hclog.Logger.
func (a *hclogAdapter) StandardLogger(opts *hclog.StandardLoggerOptions) *log.Logger {
	return log.New(a.StandardWriter(opts), "", 0)
}

// StandardWriter implements hclog.Logger: each write is one line, at INFO, or
// at opts.ForceLevel. The levels in the text are not inferred.
func (a *hclogAdapter) StandardWriter(opts *hclog.StandardLoggerOptions) io.Writer {
	level := hclog.Info
	if opts != nil && opts.ForceLevel != hclog.NoLevel {
		level = opts.ForceLevel
	}
	return lineWriter{a: a, level: level}
}

// lineWriter writes each write as a line of its logger.
type lineWriter struct {
	a     *hclogAdapter
	level hclog.Level
}

func (w lineWriter) Write(p []byte) (int, error) {
	w.a.Log(w.level, strings.TrimRight(string(p), "\r\n"))
	return len(p), nil
}
