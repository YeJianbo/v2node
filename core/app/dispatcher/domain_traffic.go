package dispatcher

import (
	"github.com/wyx2685/v2node/common/domaintraffic"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"time"
)

type domainReader struct {
	buf.Reader
	counter *domaintraffic.Counter
}

func (r *domainReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	b, err := r.Reader.ReadMultiBuffer()
	r.counter.Add(int64(b.Len()), 0)
	return b, err
}
func (r *domainReader) ReadMultiBufferTimeout(timeout time.Duration) (buf.MultiBuffer, error) {
	reader, ok := r.Reader.(buf.TimeoutReader)
	if !ok {
		return nil, buf.ErrNotTimeoutReader
	}
	b, err := reader.ReadMultiBufferTimeout(timeout)
	r.counter.Add(int64(b.Len()), 0)
	return b, err
}
func (r *domainReader) Interrupt()   { common.Interrupt(r.Reader) }
func (r *domainReader) Close() error { return common.Close(r.Reader) }

type domainWriter struct {
	buf.Writer
	counter *domaintraffic.Counter
}

func (w *domainWriter) WriteMultiBuffer(b buf.MultiBuffer) error {
	size := int64(b.Len())
	err := w.Writer.WriteMultiBuffer(b)
	if err == nil {
		w.counter.Add(0, size)
	}
	return err
}
func (w *domainWriter) Close() error { return common.Close(w.Writer) }
func (w *domainWriter) Interrupt()   { common.Interrupt(w.Writer) }
