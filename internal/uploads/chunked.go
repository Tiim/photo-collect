package uploads

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tiim/photo-collect/internal/domain"
)

// Chunked uploads let a browser on a flaky connection send a file as a series
// of small requests. Each pending upload is a file in a scratch directory that
// only grows at its current end, so after a dropped request the client asks to
// continue at the server's offset instead of starting over. Pending uploads
// live in memory: after a restart the client simply starts the file again.

var (
	ErrUploadNotFound = errors.New("upload not found")
	ErrTooManyPending = errors.New("too many unfinished uploads")
	ErrOffsetMismatch = errors.New("upload offset mismatch")
	ErrIncomplete     = errors.New("upload is incomplete")
)

// Pending is one chunked upload in progress (or recently finished).
type Pending struct {
	ID       string
	Token    string // upload link token the upload belongs to
	FolderID string
	Nickname string
	Filename string
	Size     int64 // declared total size

	path string

	// mu is held while bytes are written or the file is ingested.
	mu     sync.Mutex
	offset int64 // bytes received so far; guarded by mu
	done   bool  // ingested; guarded by mu
	result error // outcome of the ingest; guarded by mu
	gone   bool  // discarded by the sweeper; guarded by mu

	// Guarded by Chunks.mu.
	finished bool // no longer holds data on disk
	lastUsed time.Time
	abort    func() // interrupts the request currently writing, if any
	writer   uint64 // generation of the current writer
}

// Chunks holds the pending chunked uploads.
type Chunks struct {
	dir        string
	maxPending int
	ttl        time.Duration
	now        func() time.Time

	mu      sync.Mutex
	pending map[string]*Pending
}

// NewChunks keeps pending uploads in dir. At most maxPending unfinished uploads
// exist at a time; uploads untouched for ttl are discarded.
func NewChunks(dir string, maxPending int, ttl time.Duration) (*Chunks, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Chunks{dir: dir, maxPending: maxPending, ttl: ttl, now: time.Now, pending: map[string]*Pending{}}, nil
}

// Start registers a new upload of size bytes.
func (c *Chunks) Start(token, folderID, nickname, filename string, size int64) (*Pending, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()
	open := 0
	for _, p := range c.pending {
		if !p.finished {
			open++
		}
	}
	if open >= c.maxPending {
		return nil, ErrTooManyPending
	}
	id := domain.NewToken()
	path := filepath.Join(c.dir, id)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	p := &Pending{
		ID: id, Token: token, FolderID: folderID, Nickname: nickname, Filename: filename, Size: size,
		path: path, lastUsed: c.now(),
	}
	c.pending[id] = p
	return p, nil
}

// Get returns the upload with the given id if it belongs to the link token.
func (c *Chunks) Get(id, token string) (*Pending, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.pending[id]
	if !ok || p.Token != token {
		return nil, ErrUploadNotFound
	}
	p.lastUsed = c.now()
	return p, nil
}

// Write appends the bytes of r at offset, which must equal the bytes received so
// far; otherwise ErrOffsetMismatch is returned together with the server's offset.
// Bytes read before r fails are kept, so the client can resume after them.
// abort, if not nil, must make reads from r fail: it is called when another
// request for the same upload arrives while this one is still stuck reading
// from a connection the client has given up on.
func (c *Chunks) Write(p *Pending, offset int64, r io.Reader, abort func()) (int64, error) {
	c.mu.Lock()
	if p.abort != nil {
		p.abort()
	}
	c.mu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()

	c.mu.Lock()
	p.writer++
	gen := p.writer
	p.abort = abort
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if p.writer == gen {
			p.abort = nil
		}
		p.lastUsed = c.now()
		c.mu.Unlock()
	}()

	if p.gone {
		return p.offset, ErrUploadNotFound
	}
	if p.done {
		return p.offset, ErrOffsetMismatch
	}
	if offset != p.offset {
		return p.offset, ErrOffsetMismatch
	}
	f, err := os.OpenFile(p.path, os.O_WRONLY, 0)
	if err != nil {
		return p.offset, err
	}
	defer f.Close()
	if _, err := f.Seek(p.offset, io.SeekStart); err != nil {
		return p.offset, err
	}
	n, copyErr := io.Copy(f, io.LimitReader(r, p.Size-p.offset))
	p.offset += n
	if copyErr != nil {
		// A failed write may have left bytes past what was counted.
		if err := f.Truncate(p.offset); err != nil {
			return p.offset, err
		}
		return p.offset, copyErr
	}
	// Anything beyond the declared size is refused.
	var probe [1]byte
	if _, err := io.ReadFull(r, probe[:]); err == nil {
		return p.offset, ErrTooLarge
	} else if err != io.EOF {
		return p.offset, err
	}
	return p.offset, nil
}

// Complete runs ingest on the fully received file once. Later calls return the
// first outcome, so a client that lost the response can safely ask again.
func (c *Chunks) Complete(p *Pending, ingest func(io.Reader) error) error {
	c.mu.Lock()
	if p.abort != nil {
		p.abort()
	}
	c.mu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gone {
		return ErrUploadNotFound
	}
	if p.done {
		return p.result
	}
	if p.offset != p.Size {
		return ErrIncomplete
	}
	f, err := os.Open(p.path)
	if err != nil {
		return err
	}
	p.result = ingest(f)
	f.Close()
	p.done = true
	os.Remove(p.path)

	c.mu.Lock()
	p.finished = true
	p.lastUsed = c.now()
	c.mu.Unlock()
	return p.result
}

// Offset returns the number of bytes received so far.
func (p *Pending) Offset() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.offset
}

// RunSweeper discards abandoned uploads until ctx is done.
func (c *Chunks) RunSweeper(ctx context.Context) {
	t := time.NewTicker(max(c.ttl/4, time.Minute))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Sweep()
		}
	}
}

// Sweep discards uploads that have not been touched for the TTL.
func (c *Chunks) Sweep() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()
}

func (c *Chunks) sweepLocked() {
	cutoff := c.now().Add(-c.ttl)
	for id, p := range c.pending {
		if p.lastUsed.After(cutoff) {
			continue
		}
		if !p.mu.TryLock() {
			// Still stuck in a request: cut it off and discard it next time.
			if p.abort != nil {
				p.abort()
			}
			continue
		}
		p.gone = true
		if !p.done {
			os.Remove(p.path)
		}
		p.mu.Unlock()
		delete(c.pending, id)
	}
}
