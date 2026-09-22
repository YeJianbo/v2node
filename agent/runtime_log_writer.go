package agent

import (
	"io"
	"os"
	"sync"
)

type boundedRuntimeLog struct {
	mu   sync.Mutex
	file *os.File
	path string
}

// Copy-truncate keeps OpenRC's inherited descriptor valid. One backup is retained.
func (w *boundedRuntimeLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if st, err := w.file.Stat(); err == nil && st.Size() >= 8*1024*1024 {
		backup, err := os.OpenFile(w.path+".1", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return 0, err
		}
		start := max(int64(0), st.Size()-8*1024*1024)
		_, err = io.Copy(backup, io.NewSectionReader(w.file, start, st.Size()-start))
		closeErr := backup.Close()
		if err != nil {
			return 0, err
		}
		if closeErr != nil {
			return 0, closeErr
		}
		if err := os.Truncate(w.path, 0); err != nil {
			return 0, err
		}
	}
	return w.file.Write(p)
}
func (w *boundedRuntimeLog) Close() error { w.mu.Lock(); defer w.mu.Unlock(); return w.file.Close() }
func NewRuntimeLogWriter(service string) (io.WriteCloser, error) {
	return newBoundedRuntimeLog(runtimeLogPath(service))
}
func newBoundedRuntimeLog(path string) (io.WriteCloser, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	return &boundedRuntimeLog{file: f, path: path}, nil
}
func RuntimeUsesOpenRC() bool { return runtimeOpenRC() }
