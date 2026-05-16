package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
)

var (
	logger  *log.Logger
	logPath string
	logFile *os.File

	logCacheMu  sync.Mutex
	logCacheBuf atomic.Pointer[[]byte]
	logCacheGen atomic.Uint64 // bumped on every write
	logCacheVer atomic.Uint64 // generation captured in logCacheBuf
)

// teeWriter mirrors writes to stderr + log file and bumps logCacheGen so cached
// readers know the file changed.
type teeWriter struct{ w io.Writer }

func (t *teeWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	logCacheGen.Add(1)
	return n, err
}

// initLog sets up a file-backed logger at $XDG_CACHE_HOME/mt/mt.log (falling
// back to /tmp/mt.log). Output also mirrors to stderr.
func initLog() {
	cache := os.Getenv("XDG_CACHE_HOME")
	if cache == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cache = filepath.Join(home, ".cache")
		}
	}
	dir := filepath.Join(cache, "mt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		dir = "/tmp"
	}
	logPath = filepath.Join(dir, "mt.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		logPath = "/tmp/mt.log"
		f, _ = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	}
	logFile = f
	var w io.Writer = os.Stderr
	if f != nil {
		w = io.MultiWriter(os.Stderr, f)
	}
	logger = log.New(&teeWriter{w: w}, "", log.LstdFlags|log.Lmicroseconds)
	logger.Printf("---- mt starting (pid=%d, go=%s, %s/%s) ----", os.Getpid(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
	logger.Printf("log file: %s", logPath)
}

func logf(format string, args ...any) {
	if logger == nil {
		return
	}
	logger.Output(2, fmt.Sprintf(format, args...))
}

// readLog returns the current log contents, reusing a cached snapshot when no
// new lines have been written since the last read.
func readLog() ([]byte, error) {
	if logPath == "" {
		return nil, fmt.Errorf("log not initialised")
	}
	gen := logCacheGen.Load()
	if cached := logCacheBuf.Load(); cached != nil && logCacheVer.Load() == gen {
		return *cached, nil
	}
	logCacheMu.Lock()
	defer logCacheMu.Unlock()
	if cached := logCacheBuf.Load(); cached != nil && logCacheVer.Load() == gen {
		return *cached, nil
	}
	if logFile != nil {
		_ = logFile.Sync()
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		return nil, err
	}
	logCacheBuf.Store(&b)
	logCacheVer.Store(gen)
	return b, nil
}
