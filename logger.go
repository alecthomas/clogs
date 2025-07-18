// Package clogs provides a scoped, levelled, logging package where each scope is uniquely coloured. It supports subprocess
// output redirection to a scope using a PTY, and handles most common CSI (ANSI) escape sequences, so that sub-processes
// may output colour, move the cursor, and so on, and be handled correctly.
//
// It does _not_ support concurrent access yet.
package clogs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/alecthomas/clogs/csi"
	"github.com/alecthomas/types/eventsource"
	"github.com/creack/pty"
	"github.com/mattn/go-isatty"
	"golang.org/x/term"
)

type LogConfig struct {
	Level LogLevel `help:"Log level (${enum})." enum:"trace,debug,info,notice,warn,error" default:"info"`
	Debug bool     `help:"Force debug logging." xor:"level"`
	Trace bool     `help:"Force trace logging." xor:"level"`
}

type LogLevel int

const (
	LogLevelTrace LogLevel = iota
	LogLevelDebug
	LogLevelInfo
	LogLevelNotice
	LogLevelWarn
	LogLevelError
)

func (l LogLevel) String() string {
	switch l {
	case LogLevelTrace:
		return "trace"
	case LogLevelDebug:
		return "debug"
	case LogLevelInfo:
		return "info"
	case LogLevelNotice:
		return "notice"
	case LogLevelWarn:
		return "warn"
	case LogLevelError:
		return "error"
	default:
		return fmt.Sprintf("LogLevel(%d)", l)
	}
}

func (l *LogLevel) UnmarshalText(text []byte) error {
	switch string(text) {
	case "trace":
		*l = LogLevelTrace
	case "debug":
		*l = LogLevelDebug
	case "info":
		*l = LogLevelInfo
	case "notice":
		*l = LogLevelNotice
	case "warn":
		*l = LogLevelWarn
	case "error":
		*l = LogLevelError
	default:
		return fmt.Errorf("invalid log level %q", text)
	}
	return nil
}

type terminalSize struct {
	margin, width, height uint16
}

type Logger struct {
	level LogLevel
	scope string
	size  *eventsource.EventSource[terminalSize]
}

func NewLogger(config LogConfig) *Logger {
	level := config.Level
	if config.Trace {
		level = LogLevelTrace
	} else if config.Debug {
		level = LogLevelDebug
	}
	logger := &Logger{
		level: level,
		size:  eventsource.New[terminalSize](),
	}
	logger.syncTermSize()
	return logger
}

// Scope returns a new logger with the given scope.
func (l *Logger) Scope(scope string) *Logger {
	return &Logger{scope: scope, level: l.level, size: l.size}
}

func (l *Logger) getScope() string {
	// Margin is 20% of terminal.
	size := l.size.Load()
	margin := int(size.margin)
	scope := l.scope
	if len(scope) > margin {
		scope = "…" + scope[len(scope)-margin+1:]
	} else {
		scope += strings.Repeat(" ", margin-len(scope))
	}
	scope = strings.ReplaceAll(scope, "%", "%%")
	return scope
}

var ansiTable = func() map[LogLevel]string {
	if !isatty.IsTerminal(os.Stdout.Fd()) {
		return map[LogLevel]string{}
	}
	return map[LogLevel]string{
		LogLevelTrace:  "\033[90m",
		LogLevelDebug:  "\033[34m",
		LogLevelInfo:   "",
		LogLevelNotice: "\033[32m",
		LogLevelWarn:   "\033[33m",
		LogLevelError:  "\033[31m",
	}
}()

func (l *Logger) writePrefix(level LogLevel) {
	if l.level > level {
		return
	}
	fmt.Print(l.getPrefix(level))
}

func (l *Logger) getPrefix(level LogLevel) string {
	if l.level > level {
		return ""
	}
	prefix := ansiTable[level]
	scope := l.getScope()
	if scope != "" {
		prefix = targetColour(scope) + scope + "\033[0m" + "| " + prefix
	}
	return prefix
}

func (l *Logger) logf(level LogLevel, format string, args ...any) {
	if l.level > level {
		return
	}
	l.writePrefix(level)
	fmt.Printf(format+"\033[0m\n", args...)
}

func (l *Logger) Noticef(format string, args ...any) {
	l.logf(LogLevelNotice, format, args...)
}

func (l *Logger) Infof(format string, args ...any) {
	l.logf(LogLevelInfo, format, args...)
}

func (l *Logger) Debugf(format string, args ...any) {
	l.logf(LogLevelDebug, format, args...)
}

func (l *Logger) Tracef(format string, args ...any) {
	l.logf(LogLevelTrace, format, args...)
}

func (l *Logger) Warnf(format string, args ...any) {
	l.logf(LogLevelWarn, format, args...)
}

func (l *Logger) Errorf(format string, args ...any) {
	l.logf(LogLevelError, format, args...)
}

type execOptions struct {
	create      func(command string) *exec.Cmd
	afterCreate []func(*exec.Cmd)
	afterRun    []func(*exec.Cmd)
}

// ExecOption allows Exec behaviour to be configured
type ExecOption func(*execOptions)

// ExecAfterRunHook adds a function that is called after process creation.
func ExecAfterRunHook(callback func(*exec.Cmd)) ExecOption {
	return func(options *execOptions) {
		options.afterRun = append(options.afterRun, callback)
	}
}

// ExecAfterCreateHook adds a function that is called after the command is completely constructed but before it is started.
func ExecAfterCreateHook(callback func(*exec.Cmd)) ExecOption {
	return func(options *execOptions) {
		options.afterCreate = append(options.afterCreate, callback)
	}
}

// ExecCreate adds a function to create the subprocess, overriding the default.
func ExecCreate(create func(command string) *exec.Cmd) ExecOption {
	return func(options *execOptions) {
		options.create = create
	}
}

// ExecWorkingDir sets the working dir of the subprocess.
func ExecWorkingDir(dir string) ExecOption {
	return func(options *execOptions) {
		options.afterCreate = append(options.afterCreate, func(cmd *exec.Cmd) {
			cmd.Dir = dir
		})
	}
}

// Exec a command.
func (l *Logger) Exec(command string, options ...ExecOption) error {
	lines := strings.Split(command, "\n")
	for i, line := range lines {
		if i == 0 {
			l.Noticef("$ %s", line)
		} else {
			l.Noticef("  %s", line)
		}
	}

	p, t, err := pty.Open()
	if err != nil {
		return err
	}

	changes := l.size.Subscribe(nil)
	defer l.size.Unsubscribe(changes)

	// Resize the PTY to exclude the margin.
	size := l.size.Load()
	_ = pty.Setsize(p, &pty.Winsize{Rows: size.height, Cols: size.width - (l.size.Load().margin + 1)})

	go func() {
		for size := range changes {
			_ = pty.Setsize(p, &pty.Winsize{Rows: size.height, Cols: size.width - (size.margin + 1)})
		}
	}()
	defer t.Close()
	defer p.Close()
	lw := l.WriterAt(LogLevelInfo)
	defer lw.Close()

	opts := execOptions{
		create: func(command string) *exec.Cmd {
			return exec.Command("/bin/sh", "-c", command)
		},
	}
	for _, option := range options {
		option(&opts)
	}

	cmd := opts.create(command)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setsid:  true,
			Setctty: true,
		}
	}
	cmd.Stdout = t
	cmd.Stderr = t
	cmd.Stdin = t
	for _, hook := range opts.afterCreate {
		hook(cmd)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go io.Copy(lw, p) //nolint:errcheck
	for _, hook := range opts.afterRun {
		hook(cmd)
	}
	return cmd.Wait()
}

// WriterAt returns a writer that logs each line at the given level.
func (l *Logger) WriterAt(level LogLevel) io.WriteCloser {
	// Based on MIT licensed Logrus https://github.com/sirupsen/logrus/blob/bdc0db8ead3853c56b7cd1ac2ba4e11b47d7da6b/writer.go#L27
	reader, writer := io.Pipe()

	wg := &sync.WaitGroup{}
	wg.Add(1)
	go l.writerScanner(wg, reader, level)
	return &pipeWait{r: reader, PipeWriter: writer, wg: wg}
}

type pipeWait struct {
	r *io.PipeReader
	*io.PipeWriter
	wg *sync.WaitGroup
}

func (p pipeWait) Close() error {
	err := p.PipeWriter.Close()
	p.wg.Wait()
	return err
}

// There is a bit of magic here to support cursor horizontal absolute escape
// sequences. When a new position is requested, we add the width of the margin.
func (l *Logger) writerScanner(wg *sync.WaitGroup, r *io.PipeReader, level LogLevel) {
	defer r.Close()
	defer wg.Done()

	esc := csi.NewReader(r)
	drawPrefix := true
	var newline []byte
	for {
		segment, err := esc.Read()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
			os.Stdout.Write(newline)
			return
		} else if err != nil {
			os.Stdout.Write(newline)
			l.Warnf("error reading CSI sequence: %s", err)
			return
		}

		// If we have a CSI sequence, possibly intercept it to cater for the margin.
		// But in all cases we want to transform it to Text.
		if cs, ok := segment.(csi.CSI); ok {
			// All the cases we intercept are single parameter sequences.
			params, err := cs.IntParams()
			if err != nil || len(params) != 1 {
				segment = csi.Text(cs.String())
			} else {
				switch cs.Final {
				case 'G': // G is cursor horizontal absolute.
					// We have a CHA sequence, so add the margin width.
					col := params[0] + int(l.size.Load().margin) + 2
					segment = csi.Text(fmt.Sprintf("\033[%dG", col))

				case 'K': // K is erase in line. We want to intercept 1 (clear to start of line) and 2 (clear entire line).
					if params[0] == 1 || params[0] == 2 {
						// Save the cursor position.
						text := []byte("\033[s")
						// Apply the CSI.
						text = append(text, cs.String()...)
						// Move to the start of the line.
						text = append(text, "\033[1G"...)
						// Write the prefix.
						text = append(text, l.getPrefix(level)...)
						// Restore the cursor position.
						text = append(text, "\033[u"...)

						segment = csi.Text(text)
					} else {
						segment = csi.Text(cs.String())
					}
				default:
					segment = csi.Text(cs.String())
				}
			}
		}

		for _, b := range segment.(csi.Text) { //nolint:forcetypeassert
			if b == '\r' || b == '\n' {
				newline = append(newline, b)
				continue
			}
			if drawPrefix {
				os.Stdout.Write([]byte(l.getPrefix(level)))
				drawPrefix = false
			}
			for _, nl := range newline {
				os.Stdout.Write([]byte{nl})
				os.Stdout.Write([]byte(l.getPrefix(level)))
			}
			newline = nil
			os.Stdout.Write([]byte{b})
		}
	}
}

func (l *Logger) syncTermSize() {
	// Initialise terminal size.
	size := terminalSize{margin: 16, width: 80, height: 25}
	if w, h, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
		margin := uint16(max(16, w/5))                                           //nolint:gosec
		size = terminalSize{margin: margin, width: uint16(w), height: uint16(h)} //nolint:gosec
	}
	_ = l.size.Store(size)

	// Watch WINCH for changes.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(winch)
		for range winch {
			if w, h, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
				margin := uint16(max(16, w/5))                                                      //nolint:gosec
				_ = l.size.Store(terminalSize{margin: margin, width: uint16(w), height: uint16(h)}) //nolint:gosec
			}
		}
	}()
}
