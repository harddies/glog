// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package glog implements a simple level logging package based on golang's
// standard glog and glog package. It has fully compatible interface to standard
// glog package. It defines a type, Logger, with methods for formatting output.
// Basic examples:
//
//	options := glog.LogOptions{
//		File: "./abc.glog",
//		Flag: glog.StdFlags,
//		Level: glog.DebugLevel,
//		Mode: glog.RotateNone,
//	}
//	logger, err := glog.New(options)
//	if err != nil {
//		panic(err)
//	}
//	logger.Debug("hello world")
//	logger.Infof("hello, %s", "chasex")
//	logger.Warn("testing message")
//	logger.Flush()
//
//
// It also support rotating glog file by size, hour or day.
// According to rotate mode, glog file name has distinct suffix:
//
//	RotateNone: no suffix, abc.glog.
//	RotateSize: suffix with date and clock, abc.glog-YYYYMMDD-HHMMSS.
//	RotateHour: suffix with date and hour, abc.glog-YYYYMMDD-HH.
//	RotateDay: suffix with date, abc.glog-YYYYMMDD.
//
// Note that it has a daemon routine flushing buffered data to underlying file
// periodically (default every 30s). When exit, remember calling Flush() manually,
// otherwise it may cause some date loss.
package glog

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// These flags define which text to prefix to each glog entry generated by the Logger.
const (
	// Bits or'ed together to control what's printed.
	// There is no control over the order they appear (the order listed
	// here) or the format they present (as described in the comments).
	// The prefix is followed by a colon only when LongFileFlag or ShortFileFlag
	// is specified.
	// For example, flags DateFlag | TimeFlag | LevelFlag (or StdFlags) produce,
	//	2009/01/23 01:23:23 DEBUG message
	// while flags DateFlag | TimeFlag | MicroSecondFlag | LongFileFlag produce,
	//	2009/01/23 01:23:23.123123 /a/b/c/d.go:23: message

	DateFlag        = 1 << iota // the date in the local time zone: 2009/01/23
	TimeFlag                    // the time in the local time zone: 01:23:23
	MicroSecondFlag             // microsecond resolution: 01:23:23.123123.  assumes TimeFlag.
	LongFileFlag                // full file name and line number: /a/b/c/d.go:23
	ShortFileFlag               // final file name element and line number: d.go:23. overrides LongFileFlag
	LevelFlag                   // glog entry level: DEBUG, INFO, ...
	PidFlag                     // current pid
	TimeZoneFlag                // if DateFlag or TimeFlag is set, use UTC rather than the local time zone

	DefaultFlags = DateFlag | TimeFlag | LongFileFlag | LevelFlag | PidFlag // default flag
	LstdNull     = 0                                                        // no flag
)

// Level defines each glog entry's level of verbosity.
type Level int

const (
	DebugLevel Level = iota
	InfoLevel
	WarnLevel
	ErrorLevel
	FatalLevel
	PanicLevel
)

var levelName = []string{
	DebugLevel: "DEBUG",
	InfoLevel:  "INFO",
	WarnLevel:  "WARN",
	ErrorLevel: "ERROR",
	FatalLevel: "FATAL",
	PanicLevel: "PANIC",
}

// RotateMode defines glog file's rotating mode.
type RotateMode int

const (
	RotateNone RotateMode = iota // Never rotate
	RotateSize                   // Rotate file by size
	RotateHour                   // Rotate file by hour
	RotateDay                    // Rotate file by day
)

// LogOptions control logger's behaviour.
type LogOptions struct {
	LogDir  string     //  name for glog file
	LogName string     //  name for glog file
	Flag    int        // glog entry prefix flag
	Level   Level      // threshold level for logging
	Mode    RotateMode // file rotating mode
	Maxsize uint64     // maximum size for RotateSize mode	(Megabytes)
}

// A Logger represents an active logging object that generates lines of
// output to an io.Writer.  Each logging operation makes a single call to
// the Writer's Write method.  A Logger can be used simultaneously from
// multiple goroutines; it guarantees to serialize access to the Writer.
type Logger struct {
	options    LogOptions
	freeList   *buffer
	freeListMu sync.Mutex

	mu     sync.Mutex    // ensures atomic writes; protects the following fields
	out    *bufio.Writer // destination for output
	file   *os.File
	nbytes uint64
	hour   int
	day    int
}

type buffer struct {
	buf  []byte
	next *buffer
}

// getBuffer returns a new, ready-to-use buffer.
func (l *Logger) getBuffer() *buffer {
	l.freeListMu.Lock()
	b := l.freeList
	if b != nil {
		l.freeList = b.next
	}
	l.freeListMu.Unlock()
	if b == nil {
		b = &buffer{buf: make([]byte, 64)}
	} else {
		b.next = nil
	}
	return b
}

// putBuffer returns a buffer to the free list.
func (l *Logger) putBuffer(b *buffer) {
	if len(b.buf) >= 256 {
		// Let big buffers die a natural death.
		return
	}
	l.freeListMu.Lock()
	b.next = l.freeList
	l.freeList = b
	l.freeListMu.Unlock()
}

// New creates a new Logger.   The out variable sets the
// destination to which glog data will be written.
// The prefix appears at the beginning of each generated glog line.
// The flag argument defines the logging properties.
func New(options LogOptions) (*Logger, error) {
	logger := &Logger{options: options}

	err := logger.createFile(time.Now())
	if err != nil {
		return logger, err
	}
	go logger.flushDaemon()

	return logger, nil
}

// bufferSize sizes the buffer associated with each glog file. It's large
// so that glog records can accumulate without the logging thread blocking
// on disk I/O. The flushDaemon will block instead.
const bufferSize = 256 * 1024

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// createFile creates glog file with specified timestamp.
// l.mu held
func (l *Logger) createFile(t time.Time) error {
	exist, err := pathExists(l.options.LogDir)
	if err != nil {
		fmt.Printf("get log dir error![%v]\n", err)
	}
	if !exist {
		// create dir
		err := os.Mkdir(l.options.LogDir, os.ModePerm)
		if err != nil {
			fmt.Printf("create log dir [%v] fail! [%v]\n", l.options.LogDir, err)
		} else {
			fmt.Printf("create log dir [%v] success!\n", l.options.LogDir)
		}
	}

	year, month, day := t.Date()
	hour, min, sec := t.Clock()

	var file string
	switch l.options.Mode {
	case RotateSize:
		file = fmt.Sprintf("%s/%04d%02d%02d-%02d%02d%02d-%s", strings.TrimRight(l.options.LogDir, "/"), year, month, day, hour, min, sec, l.options.LogName)
	case RotateHour:
		file = fmt.Sprintf("%s/%04d%02d%02d-%02d-%s", strings.TrimRight(l.options.LogDir, "/"), year, month, day, hour, l.options.LogName)
	case RotateDay:
		file = fmt.Sprintf("%s/%04d%02d%02d-%s", strings.TrimRight(l.options.LogDir, "/"), year, month, day, l.options.LogName)
	default: // RotateNone
		file = fmt.Sprintf("%s/%s", strings.TrimRight(l.options.LogDir, "/"), l.options.LogName)
	}

	if l.file != nil {
		l.out.Flush()
		l.file.Close()
	}

	f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0664)
	if err != nil {
		return err
	}

	l.file = f
	l.out = bufio.NewWriterSize(f, bufferSize)
	l.nbytes = 0
	l.hour = hour
	l.day = day

	return nil
}

// Cheap integer to fixed-width decimal ASCII.  Give a negative width to avoid zero-padding.
func itoa(buf *[]byte, i int, wid int) {
	// Assemble decimal in reverse order.
	var b [20]byte
	bp := len(b) - 1
	for i >= 10 || wid > 1 {
		wid--
		q := i / 10
		b[bp] = byte('0' + i - q*10)
		bp--
		i = q
	}
	// i < 10
	b[bp] = byte('0' + i)
	*buf = append(*buf, b[bp:]...)
}

// 格式化日志头
func (l *Logger) formatHeader(buf *[]byte, s Level, calldepth int, t time.Time) {
	flag := l.options.Flag

	if flag&TimeZoneFlag != 0 {
		t = t.UTC()
	}

	if flag&(DateFlag|TimeFlag|MicroSecondFlag) != 0 {
		if flag&DateFlag != 0 {
			year, month, day := t.Date()
			itoa(buf, year, 4)
			*buf = append(*buf, '-')
			itoa(buf, int(month), 2)
			*buf = append(*buf, '-')
			itoa(buf, day, 2)
			*buf = append(*buf, ' ')
		}
		if flag&(TimeFlag|MicroSecondFlag) != 0 {
			hour, min, sec := t.Clock()
			itoa(buf, hour, 2)
			*buf = append(*buf, ':')
			itoa(buf, min, 2)
			*buf = append(*buf, ':')
			itoa(buf, sec, 2)
			if flag&MicroSecondFlag != 0 {
				*buf = append(*buf, '.')
				itoa(buf, t.Nanosecond()/1e3, 6)
			}
			*buf = append(*buf, ' ')
		}
	}

	if flag&(ShortFileFlag|LongFileFlag) != 0 {
		_, file, line, ok := runtime.Caller(calldepth)
		if !ok {
			file = "???"
			line = 0
		}

		if flag&ShortFileFlag != 0 {
			short := file
			for i := len(file) - 1; i > 0; i-- {
				if file[i] == '/' {
					short = file[i+1:]
					break
				}
			}
			file = short
		}
		*buf = append(*buf, file...)
		*buf = append(*buf, ':')
		itoa(buf, line, -1)
		*buf = append(*buf, ' ')
	}

	if flag&PidFlag != 0 {
		*buf = append(*buf, '[')
		itoa(buf, os.Getpid(), -1)
		*buf = append(*buf, ']')
		*buf = append(*buf, ' ')
	}

	if flag&LevelFlag != 0 {
		*buf = append(*buf, levelName[s]...)
		*buf = append(*buf, ' ')
	}
}

// Output writes the output for a logging event.  The string s contains
// the text to print after the prefix specified by the flags of the
// Logger.  A newline is appended if the last character of s is not
// already a newline.  Calldepth is used to recover the PC and is
// provided for generality, although at the moment on all pre-defined
// paths it will be 2.
func (l *Logger) Output(level Level, calldepth int, s string) error {
	b := l.getBuffer()
	defer l.putBuffer(b)

	now := time.Now() // get this early.

	b.buf = b.buf[:0]
	l.formatHeader(&b.buf, level, calldepth, now)
	b.buf = append(b.buf, s...)
	if len(s) == 0 || s[len(s)-1] != '\n' {
		b.buf = append(b.buf, '\n')
	}

	l.mu.Lock()
	rotate := false
	switch l.options.Mode {
	case RotateSize:
		if l.nbytes+uint64(len(b.buf)) > l.options.Maxsize*1024*1024 {
			rotate = true
		}
	case RotateHour:
		if l.hour != now.Hour() || l.day != now.Day() {
			rotate = true
		}
	case RotateDay:
		if l.day != now.Day() {
			rotate = true
		}
	}

	if rotate {
		if err := l.createFile(now); err != nil {
			fmt.Fprintf(os.Stderr, "glog: exiting because of error: %s\n", err)
			os.Exit(2)
		}
	}
	_, err := l.out.Write(b.buf)
	l.nbytes += uint64(len(b.buf))

	if l.options.Level == DebugLevel {
		os.Stdout.Write(b.buf)
	}

	if level == FatalLevel || level == PanicLevel {
		l.out.Flush()
		l.file.Close()
		l.mu.Unlock()

		if level == PanicLevel {
			panic(s)
		}
		os.Exit(1)
	}

	l.mu.Unlock()
	return err
}

// stacks is a wrapper for runtime.Stack that attempts to recover the data for all goroutines.
func stacks(all bool) []byte {
	// We don't know how big the traces are, so grow a few times if they don't fit. Start large, though.
	n := 10000
	if all {
		n = 100000
	}
	var trace []byte
	for i := 0; i < 5; i++ {
		trace = make([]byte, n)
		nbytes := runtime.Stack(trace, all)
		if nbytes < len(trace) {
			return trace[:nbytes]
		}
		n *= 2
	}
	return trace
}

// Flush flush buffered data to underlying file and sync contents to stable storage.
func (l *Logger) Flush() {
	l.mu.Lock()
	l.out.Flush()
	l.file.Sync()
	l.mu.Unlock()
}

// 每隔30s将
const flushInterval = 30 * time.Second

// flushDaemon periodically flushes the glog file buffers.
func (l *Logger) flushDaemon() {
	for _ = range time.NewTicker(flushInterval).C {
		l.Flush()
	}
}

// Debugf calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Printf.
func (l *Logger) Debugf(format string, v ...interface{}) {
	if l.options.Level <= DebugLevel {
		l.Output(DebugLevel, 3, fmt.Sprintf(format, v...))
	}
}

// Debug calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Print.
func (l *Logger) Debug(v ...interface{}) {
	if l.options.Level <= DebugLevel {
		l.Output(DebugLevel, 3, fmt.Sprint(v...))
	}
}

// Debugln calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Println.
func (l *Logger) Debugln(v ...interface{}) {
	if l.options.Level <= DebugLevel {
		l.Output(DebugLevel, 3, fmt.Sprintln(v...))
	}
}

// Infof calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Printf.
func (l *Logger) Infof(format string, v ...interface{}) {
	if l.options.Level <= InfoLevel {
		l.Output(InfoLevel, 3, fmt.Sprintf(format, v...))
	}
}

// Info calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Print.
func (l *Logger) Info(v ...interface{}) {
	if l.options.Level <= InfoLevel {
		l.Output(InfoLevel, 3, fmt.Sprint(v...))
	}
}

// Infoln calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Println.
func (l *Logger) Infoln(v ...interface{}) {
	if l.options.Level <= InfoLevel {
		l.Output(InfoLevel, 3, fmt.Sprintln(v...))
	}
}

// Warnf calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Printf.
func (l *Logger) Warnf(format string, v ...interface{}) {
	if l.options.Level <= WarnLevel {
		l.Output(WarnLevel, 3, fmt.Sprintf(format, v...))
	}
}

// Warn calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Print.
func (l *Logger) Warn(v ...interface{}) {
	if l.options.Level <= WarnLevel {
		l.Output(WarnLevel, 3, fmt.Sprint(v...))
	}
}

// Warnln calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Println.
func (l *Logger) Warnln(v ...interface{}) {
	if l.options.Level <= WarnLevel {
		l.Output(WarnLevel, 3, fmt.Sprintln(v...))
	}
}

// Errorf calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Printf.
func (l *Logger) Errorf(format string, v ...interface{}) {
	if l.options.Level <= ErrorLevel {
		l.Output(ErrorLevel, 3, fmt.Sprintf(format, v...))
	}
}

// Error calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Print.
func (l *Logger) Error(v ...interface{}) {
	if l.options.Level <= ErrorLevel {
		l.Output(ErrorLevel, 3, fmt.Sprint(v...))
	}
}

// Errorln calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Println.
func (l *Logger) Errorln(v ...interface{}) {
	if l.options.Level <= ErrorLevel {
		l.Output(ErrorLevel, 3, fmt.Sprintln(v...))
	}
}

// Fatal is equivalent to l.Print() followed by a call to os.Exit(1).
func (l *Logger) Fatal(v ...interface{}) {
	l.Output(FatalLevel, 2, fmt.Sprint(v...))
}

// Fatalf is equivalent to l.Printf() followed by a call to os.Exit(1).
func (l *Logger) Fatalf(format string, v ...interface{}) {
	l.Output(FatalLevel, 2, fmt.Sprintf(format, v...))
}

// Fatalln is equivalent to l.Println() followed by a call to os.Exit(1).
func (l *Logger) Fatalln(v ...interface{}) {
	l.Output(FatalLevel, 2, fmt.Sprintln(v...))
}

// Panic is equivalent to l.Print() followed by a call to panic().
func (l *Logger) Panic(v ...interface{}) {
	l.Output(PanicLevel, 2, fmt.Sprint(v...))
}

// Panicf is equivalent to l.Printf() followed by a call to panic().
func (l *Logger) Panicf(format string, v ...interface{}) {
	l.Output(PanicLevel, 2, fmt.Sprintf(format, v...))
}

// Panicln is equivalent to l.Println() followed by a call to panic().
func (l *Logger) Panicln(v ...interface{}) {
	l.Output(PanicLevel, 2, fmt.Sprintln(v...))
}
