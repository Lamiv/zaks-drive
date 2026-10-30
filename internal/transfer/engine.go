// Package transfer moves document bytes between callers and SharePoint. It
// picks single-request or chunked upload sessions, bounds memory with a
// process-wide budget, spools bodies of unknown length to disk, and streams
// downloads with Range support.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/Lamiv/zaks-drive/internal/apperr"
	"github.com/Lamiv/zaks-drive/internal/graph"
	"github.com/Lamiv/zaks-drive/internal/observability"
	"github.com/Lamiv/zaks-drive/internal/safelog"
)

var ErrTooLarge = errors.New("upload exceeds maximum size")

type Options struct {
	SimpleUploadMax int64
	ChunkSize       int64
	MemoryBudget    int64
	MaxUpload       int64
	SpoolDir        string
}

// Every transfer buffer (upload, chunk and download copy buffers) is
// reserved against MemoryBudget for its full allocated size before use, so
// the budget is a hard ceiling on transfer buffer memory. Small uploads use
// size classes so a 50 KiB upload does not pin a 4 MiB buffer.
type Engine struct {
	g      *graph.Client
	opts   Options
	budget *semaphore.Weighted

	classes []bufClass // ascending sizes, last = SimpleUploadMax
	chunk   bufClass   // ChunkSize, for upload sessions
	copyBuf bufClass   // streaming downloads
}

type bufClass struct {
	size int64
	pool *sync.Pool
}

func newClass(size int64) bufClass {
	return bufClass{size: size, pool: &sync.Pool{New: func() any { b := make([]byte, size); return &b }}}
}

const copyBufSize = 256 << 10

func New(g *graph.Client, opts Options) *Engine {
	e := &Engine{g: g, opts: opts, budget: semaphore.NewWeighted(opts.MemoryBudget)}
	for _, sz := range []int64{64 << 10, 512 << 10} {
		if sz < opts.SimpleUploadMax {
			e.classes = append(e.classes, newClass(sz))
		}
	}
	e.classes = append(e.classes, newClass(opts.SimpleUploadMax))
	e.chunk = newClass(opts.ChunkSize)
	e.copyBuf = newClass(min(copyBufSize, opts.MemoryBudget))
	return e
}

// buffer reserves budget for, and returns, a pooled buffer of class c.
func (e *Engine) buffer(ctx context.Context, c bufClass) (*[]byte, func(), error) {
	release, err := e.reserve(ctx, c.size)
	if err != nil {
		return nil, nil, err
	}
	bp := c.pool.Get().(*[]byte)
	return bp, func() { c.pool.Put(bp); release() }, nil
}

func (e *Engine) classFor(n int64) bufClass {
	for _, c := range e.classes {
		if n <= c.size {
			return c
		}
	}
	return e.classes[len(e.classes)-1]
}

// Target identifies where an upload goes: a new file by path, or new
// content for an existing item.
type Target struct {
	Drive    string
	Path     string // drive-relative path incl. file name (new file)
	ItemID   string // existing item (replace content)
	Conflict graph.ConflictBehavior
	IfMatch  string // optimistic concurrency: for path targets only honoured by single-request uploads
}

func (e *Engine) reserve(ctx context.Context, n int64) (func(), error) {
	if !e.budget.TryAcquire(n) {
		start := time.Now()
		if err := e.budget.Acquire(ctx, n); err != nil {
			return nil, err
		}
		observability.TransferBufferWait.Observe(time.Since(start).Seconds())
	}
	return func() { e.budget.Release(n) }, nil
}

// Upload stores body at t. size is the exact body length, or -1 if unknown.
func (e *Engine) Upload(ctx context.Context, t Target, body io.Reader, size int64) (*graph.DriveItem, error) {
	if size > e.opts.MaxUpload {
		return nil, e.tooLarge(size)
	}
	if size >= 0 && size <= e.opts.SimpleUploadMax {
		return e.uploadSimple(ctx, t, body, size)
	}
	if size > 0 {
		return e.uploadChunked(ctx, t, body, size)
	}
	return e.uploadUnknown(ctx, t, body)
}

func (e *Engine) uploadSimple(ctx context.Context, t Target, body io.Reader, size int64) (*graph.DriveItem, error) {
	bp, done, err := e.buffer(ctx, e.classFor(size))
	if err != nil {
		return nil, err
	}
	defer done()
	buf := (*bp)[:size]
	if _, err := io.ReadFull(body, buf); err != nil {
		return nil, fmt.Errorf("read upload body: %w", err)
	}
	return e.putSmall(ctx, t, buf)
}

func (e *Engine) putSmall(ctx context.Context, t Target, data []byte) (*graph.DriveItem, error) {
	observability.TransfersActive.WithLabelValues("upload", "simple").Inc()
	defer observability.TransfersActive.WithLabelValues("upload", "simple").Dec()
	var it *graph.DriveItem
	var err error
	if t.ItemID != "" {
		it, err = e.g.ReplaceSmall(ctx, t.Drive, t.ItemID, data, t.IfMatch)
	} else {
		it, err = e.g.UploadSmallByPath(ctx, t.Drive, t.Path, t.Conflict, data, t.IfMatch)
	}
	if err == nil {
		observability.BytesTransferred.WithLabelValues("upload").Add(float64(len(data)))
	}
	return it, err
}

// uploadChunked streams body through one reusable chunk buffer, so memory
// per transfer is ChunkSize regardless of file size.
func (e *Engine) uploadChunked(ctx context.Context, t Target, body io.Reader, size int64) (*graph.DriveItem, error) {
	var sess *graph.UploadSession
	var err error
	if t.ItemID != "" {
		sess, err = e.g.CreateUploadSessionForItem(ctx, t.Drive, t.ItemID, t.IfMatch)
	} else {
		sess, err = e.g.CreateUploadSessionByPath(ctx, t.Drive, t.Path, t.Conflict)
	}
	if err != nil {
		return nil, err
	}
	observability.TransfersActive.WithLabelValues("upload", "session").Inc()
	defer observability.TransfersActive.WithLabelValues("upload", "session").Dec()

	bp, done, err := e.buffer(ctx, e.chunk)
	if err != nil {
		e.g.CancelUploadSession(context.WithoutCancel(ctx), sess)
		return nil, err
	}
	defer done()

	var offset int64
	for offset < size {
		n := min(e.opts.ChunkSize, size-offset)
		buf := (*bp)[:n]
		if _, err := io.ReadFull(body, buf); err != nil {
			e.g.CancelUploadSession(context.WithoutCancel(ctx), sess)
			return nil, fmt.Errorf("read upload body at %d: %w", offset, err)
		}
		it, err := e.g.UploadChunk(ctx, sess, offset, buf, size)
		if err != nil {
			e.g.CancelUploadSession(context.WithoutCancel(ctx), sess)
			return nil, err
		}
		observability.BytesTransferred.WithLabelValues("upload").Add(float64(n))
		offset += n
		if it != nil && it.ID != "" {
			return it, nil
		}
	}
	return nil, errors.New("upload session finished without returning an item")
}

// uploadUnknown handles bodies without Content-Length (chunked encoding or
// multipart parts): small bodies stay in memory, large ones spool to disk.
func (e *Engine) uploadUnknown(ctx context.Context, t Target, body io.Reader) (*graph.DriveItem, error) {
	bp, done, err := e.buffer(ctx, e.classes[len(e.classes)-1])
	if err != nil {
		return nil, err
	}
	buf := *bp
	n, err := io.ReadFull(body, buf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		defer done()
		return e.putSmall(ctx, t, buf[:n])
	}
	if err != nil {
		done()
		return nil, fmt.Errorf("read upload body: %w", err)
	}

	f, err := os.CreateTemp(e.opts.SpoolDir, "upload-*")
	if err != nil {
		done()
		return nil, fmt.Errorf("spool: %w", err)
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	_, werr := f.Write(buf[:n])
	done()
	if werr != nil {
		return nil, fmt.Errorf("spool: %w", werr)
	}
	limit := e.opts.MaxUpload - int64(n)
	copied, err := io.Copy(f, io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	if copied > limit {
		return nil, e.tooLarge(-1)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return e.uploadChunked(ctx, t, f, int64(n)+copied)
}

// ---- downloads ----

// DownloadResult describes what was written to the client.
type DownloadResult struct {
	Status int
	Bytes  int64
}

// passthroughHeaders are copied from SharePoint to the caller.
var passthroughHeaders = []string{"Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified"}

// Stream copies the item content (optionally a byte range) to w. Response
// headers set by the caller (Content-Type, ETag, Content-Disposition) are
// kept. refresh is called once to obtain a new download URL if the cached
// one has expired.
func (e *Engine) Stream(ctx context.Context, w http.ResponseWriter, downloadURL, rangeHdr string, refresh func() (string, error)) (DownloadResult, error) {
	resp, err := e.Open(ctx, downloadURL, rangeHdr, refresh)
	if err != nil {
		return DownloadResult{}, err
	}
	return e.StreamOpened(ctx, w, resp)
}

// StreamOpened writes an open content response (status, range headers and
// body) to w and closes it. Content-Type and other descriptive headers set
// by the caller are kept.
func (e *Engine) StreamOpened(ctx context.Context, w http.ResponseWriter, resp *http.Response) (DownloadResult, error) {
	defer resp.Body.Close()
	for _, h := range passthroughHeaders {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if w.Header().Get("Accept-Ranges") == "" {
		w.Header().Set("Accept-Ranges", "bytes")
	}
	w.WriteHeader(resp.StatusCode)
	n, err := e.Copy(ctx, w, resp.Body)
	return DownloadResult{Status: resp.StatusCode, Bytes: n}, err
}

// Open starts a content download (optionally a byte range) and returns the
// raw response for callers that need full control over the reply. The
// caller closes the body.
func (e *Engine) Open(ctx context.Context, downloadURL, rangeHdr string, refresh func() (string, error)) (*http.Response, error) {
	resp, err := e.g.OpenDownload(ctx, downloadURL, rangeHdr)
	if err != nil && refresh != nil && isExpired(err) {
		if downloadURL, err = refresh(); err == nil {
			resp, err = e.g.OpenDownload(ctx, downloadURL, rangeHdr)
		}
	}
	return resp, err
}

// Copy streams src to dst through a pooled, budgeted buffer and records metrics.
func (e *Engine) Copy(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	bp, done, err := e.buffer(ctx, e.copyBuf)
	if err != nil {
		return 0, err
	}
	defer done()
	observability.TransfersActive.WithLabelValues("download", "stream").Inc()
	defer observability.TransfersActive.WithLabelValues("download", "stream").Dec()
	n, err := io.CopyBuffer(writerOnly{dst}, src, *bp)
	observability.BytesTransferred.WithLabelValues("download").Add(float64(n))
	return n, err
}

// StreamResponse copies an already-open Graph response (e.g. thumbnails).
func (e *Engine) StreamResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response) (int64, error) {
	defer resp.Body.Close()
	for _, h := range append(passthroughHeaders, "Content-Type") {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	return e.Copy(ctx, w, resp.Body)
}

// isExpired reports whether a pre-authenticated URL was rejected because
// its embedded token expired.
func isExpired(err error) bool {
	s := graph.StatusOf(err)
	return s == http.StatusUnauthorized || s == http.StatusForbidden || s == http.StatusNotFound
}

// writerOnly hides ReadFrom so io.CopyBuffer uses our pooled buffer.
type writerOnly struct{ io.Writer }

// tooLarge builds the coded error; size is -1 when the body had no length.
func (e *Engine) tooLarge(size int64) error {
	d := fmt.Sprintf("the upload is larger than the limit of %d bytes (transfer.maxUploadBytes)", e.opts.MaxUpload)
	if size >= 0 {
		d = fmt.Sprintf("the upload is %d bytes; the limit is %d bytes (transfer.maxUploadBytes)", size, e.opts.MaxUpload)
	}
	return apperr.New(apperr.UploadTooLarge, "%s", d).Wrap(ErrTooLarge)
}

func init() { safelog.Allow(ErrTooLarge) }

// MaxUpload is the largest accepted upload in bytes (transfer.maxUploadBytes).
func (e *Engine) MaxUpload() int64 { return e.opts.MaxUpload }
