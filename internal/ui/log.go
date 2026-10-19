package ui

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/roman-16/proton-cli/internal/errs"
	"github.com/roman-16/proton-cli/internal/redact"
)

// undeclared is what stands in for an attribute nobody declared a policy for.
//
// A log line is written once and read by somebody who was not there, so the
// worst answer to an unknown attribute is to write it and hope. Refusing it
// leaves the name visible - which is what makes the omission obvious to whoever
// reads the log, and to the conformance test that fails the build over it -
// while the value stays where it was.
const undeclared = "<undeclared>"

// redacting is a handler that applies the declared policy to every attribute on
// its way through, and hands the result to another handler to format.
//
// It wraps rather than formats because the two destinations want different
// shapes - a person reading stderr wants the text form, a report wants JSON -
// and neither may see anything the other would not. Putting the redaction above
// both is what makes that true by construction instead of twice.
type redacting struct {
	inner    slog.Handler
	redactor *redact.Redactor
	// shared says somebody other than the person the run happened in front of
	// will read this, which is what the message itself is rewritten for.
	//
	// A record's attributes are named, so a policy can be declared for each and
	// held to wherever it goes. Its message is one sentence assembled by whoever
	// wrote it, and a sentence has no name to declare anything about - so the only
	// thing that can be done to it is the weakest policy, and the only place worth
	// doing it is the destination that leaves the machine. The screen keeps the
	// sentence whole: a warning there is the run speaking to the person whose
	// account it is, and an address of theirs on their own terminal is theirs.
	shared bool
}

func (h redacting) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h redacting) Handle(ctx context.Context, r slog.Record) error {
	message := r.Message
	if h.shared {
		message = h.redactor.Text(message)
	}
	clean := slog.NewRecord(r.Time, r.Level, message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		if attr, ok := h.attr(a); ok {
			clean.AddAttrs(attr)
		}
		return true
	})
	return h.inner.Handle(ctx, clean)
}

// attr applies one attribute's policy.
//
// A group is walked rather than refused, so that a caller reaching for
// slog.Group is held to the same vocabulary as one passing a flat pair.
func (h redacting) attr(a slog.Attr) (slog.Attr, bool) {
	if a.Value.Kind() == slog.KindGroup {
		group := a.Value.Group()
		kept := make([]slog.Attr, 0, len(group))
		for _, inner := range group {
			if clean, ok := h.attr(inner); ok {
				kept = append(kept, clean)
			}
		}
		if len(kept) == 0 {
			return slog.Attr{}, false
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(kept...)}, true
	}
	if !redact.Declared(a.Key) {
		return slog.String(a.Key, undeclared), true
	}
	// A number, a boolean or a duration describes the machinery, and the policy
	// for such an attribute is Keep by declaration. Rendering it to a string to
	// pass it through the redactor and back would turn a JSON number into a
	// quoted one for nothing.
	switch a.Value.Kind() {
	case slog.KindBool, slog.KindDuration, slog.KindFloat64, slog.KindInt64, slog.KindTime, slog.KindUint64:
		if redact.Fields[a.Key] == redact.Keep {
			return a, true
		}
	}
	value, keep := h.redactor.Apply(a.Key, stringify(a.Value))
	if !keep {
		return slog.Attr{}, false
	}
	return slog.String(a.Key, value), true
}

// stringify renders a value for redaction. Anything that is not already text is
// asked for its own string form, which is what an error, a stringer and a
// number all answer to.
//
// An error is asked for the form that withholds the account's own names, because
// this is where an error stops being a value a command may still phrase for its
// reader and becomes a line in a file.
func stringify(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindAny:
		switch value := v.Any().(type) {
		case nil:
			return ""
		case error:
			return errs.Withheld(value)
		}
	}
	return v.String()
}

func (h redacting) WithAttrs(attrs []slog.Attr) slog.Handler {
	kept := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		if clean, ok := h.attr(a); ok {
			kept = append(kept, clean)
		}
	}
	h.inner = h.inner.WithAttrs(kept)
	return h
}

func (h redacting) WithGroup(name string) slog.Handler {
	h.inner = h.inner.WithGroup(name)
	return h
}

// logcat renders screen records in a compact, single-line Android logcat style.
// The daily diagnostic file keeps its structured JSON representation.
type logcat struct {
	w      io.Writer
	mu     *sync.Mutex
	level  slog.Level
	attrs  []slog.Attr
	groups []string
}

func newLogcat(w io.Writer, level slog.Level) slog.Handler {
	return &logcat{w: w, mu: &sync.Mutex{}, level: level}
}

func (h *logcat) Enabled(_ context.Context, level slog.Level) bool { return level >= h.level }

func (h *logcat) Handle(_ context.Context, r slog.Record) error {
	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	var line strings.Builder
	fmt.Fprintf(&line, "%s %d %c cli: %s", t.Local().Format("01-02 15:04:05.000"), os.Getpid(), logcatLevel(r.Level), r.Message)
	var writeAttr func(slog.Attr, []string)
	writeAttr = func(a slog.Attr, groups []string) {
		a.Value = a.Value.Resolve()
		if a.Value.Kind() == slog.KindGroup {
			next := append(append([]string(nil), groups...), a.Key)
			for _, child := range a.Value.Group() {
				writeAttr(child, next)
			}
			return
		}
		keyParts := append(append([]string(nil), groups...), a.Key)
		value := a.Value.String()
		if a.Value.Kind() == slog.KindString && strings.ContainsAny(value, " \t\r\n=\"\\") {
			value = strconv.Quote(value)
		}
		fmt.Fprintf(&line, " %s=%s", strings.Join(keyParts, "."), value)
	}
	for _, a := range h.attrs {
		writeAttr(a, h.groups)
	}
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(a, h.groups)
		return true
	})
	line.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, line.String())
	return err
}

func logcatLevel(level slog.Level) byte {
	switch {
	case level >= slog.LevelError:
		return 'E'
	case level >= slog.LevelWarn:
		return 'W'
	case level >= slog.LevelInfo:
		return 'I'
	default:
		return 'D'
	}
}

func (h *logcat) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &clone
}

func (h *logcat) WithGroup(name string) slog.Handler {
	clone := *h
	clone.groups = append(append([]string(nil), h.groups...), name)
	return &clone
}

// fanout sends every record to each of several handlers, each with its own idea
// of what is worth reporting.
//
// The two levels are the whole point. What reaches stderr is a preference - most
// people want warnings and worse - and what reaches the file is not: a run that
// only recorded what was already on the screen would be no more use after the
// fact than the screen was.
type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	var err error
	for _, h := range f {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if e := h.Handle(ctx, r.Clone()); e != nil && err == nil {
			err = e
		}
	}
	return err
}

func (f fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (f fanout) WithGroup(name string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}

// newLoggers builds the two an invocation has, behind one redactor so that a
// handle means the same thing in each.
//
// Log is for what happens while the work is being done - a retry, a signature
// that would not verify, an item that would not open - and goes to both: the
// screen at whatever verbosity was asked for, the file always and in full.
//
// Trace is for the run's own envelope: what was invoked, how it came out, the
// stack if it crashed. That goes to the file alone, because the screen has its
// own way of saying all three and saying them twice is how a failure comes to
// be reported to a person as two different-looking problems.
//
// Every record the file receives names the run, because a day's file holds every
// run of that day and is read back as the runs that made it up. The name is
// attached here rather than passed at each call, so that a line written anywhere
// - a service, a request, a skip - belongs to its run by construction. The
// screen is left alone: there is only ever one run on it.
func newLoggers(errw io.Writer, level slog.Level, salt []byte, file io.Writer, run string) (log, trace *slog.Logger) {
	redactor := redact.New(salt)
	screen := redacting{
		redactor: redactor,
		inner:    newLogcat(errw, level),
	}
	if file == nil {
		return slog.New(screen), slog.New(slog.DiscardHandler)
	}
	recorded := redacting{
		redactor: redactor,
		shared:   true,
		inner:    slog.NewJSONHandler(file, &slog.HandlerOptions{Level: slog.LevelDebug}),
	}.WithAttrs([]slog.Attr{slog.String("run", run)})
	return slog.New(fanout{screen, recorded}), slog.New(recorded)
}
