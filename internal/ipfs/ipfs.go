// Package ipfs is a minimal kubo RPC client: add-and-pin, pin, unpin, list pins, and cat.
// The hub owns all pinned content (v1 is hub-mediated upload only), so cat
// only ever streams CIDs the hub itself pinned — no untrusted fetches.
package ipfs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type Client struct {
	api    string // e.g. http://127.0.0.1:5001
	client *http.Client
	// stream serves cat: a video read at a phone's pace outlasts any
	// whole-request timeout, so only the wait for kubo's headers is
	// bounded and the body lives as long as the caller's context
	stream *http.Client
}

func New(api string) *Client {
	return &Client{api: api, client: &http.Client{Timeout: 60 * time.Second},
		stream: &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 30 * time.Second}}}
}

// Available pings the kubo API; embeds degrade to 503 without it.
func (c *Client) Available() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", c.api+"/api/v0/version", nil)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Add stores and pins the bytes as a CIDv1, returning the CID. kubo's add
// pins the root only after it has written the file's JSON object, so the
// response is read to its end: hanging up on the first object cancels the
// request on kubo's side and the pin is lost while the blocks stay (that
// race left 63 of 152 uploads unpinned by 2026-09-09).
func (c *Client) Add(r io.Reader, name string) (string, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("file", name)
		if err == nil {
			_, err = io.Copy(part, r)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	resp, err := c.client.Post(c.api+"/api/v0/add?pin=true&cid-version=1", mw.FormDataContentType(), pr)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("ipfs add: %s: %s", resp.Status, b)
	}
	// one object per file (more with progress or wrapping); the stream ends
	// only when the command, root pin included, has finished
	dec := json.NewDecoder(resp.Body)
	var hash string
	for {
		var out struct {
			Hash string `json:"Hash"`
		}
		if err := dec.Decode(&out); err == io.EOF {
			break
		} else if err != nil {
			return "", fmt.Errorf("ipfs add: %w", err)
		}
		if out.Hash != "" {
			hash = out.Hash
		}
	}
	if hash == "" {
		return "", fmt.Errorf("ipfs add: no hash in response")
	}
	return hash, nil
}

// Pin pins a CID the node already holds; idempotent. The reconcile pass
// uses it for uploads kubo has the blocks of but no pin.
func (c *Client) Pin(cid string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", c.api+"/api/v0/pin/add?arg="+url.QueryEscape(cid)+"&recursive=true", nil)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("ipfs pin/add %s: %s", cid, resp.Status)
	}
	return nil
}

// Pinned lists the node's recursive pins.
func (c *Client) Pinned() (map[string]bool, error) {
	resp, err := c.client.Post(c.api+"/api/v0/pin/ls?type=recursive", "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("ipfs pin/ls: %s: %s", resp.Status, b)
	}
	var out struct {
		Keys map[string]json.RawMessage `json:"Keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("ipfs pin/ls: %w", err)
	}
	set := make(map[string]bool, len(out.Keys))
	for k := range out.Keys {
		set[k] = true
	}
	return set, nil
}

// Unpin is best-effort garbage collection; a failed unpin only wastes disk.
func (c *Client) Unpin(cid string) error {
	resp, err := c.client.Post(c.api+"/api/v0/pin/rm?arg="+url.QueryEscape(cid), "", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("ipfs pin/rm %s: %s", cid, resp.Status)
	}
	return nil
}

// Cat streams a pinned CID's bytes, or length bytes of them from offset
// when length > 0 (kubo's cat reads only the blocks that span). The body
// lives until ctx ends or the caller closes the reader, which it must.
func (c *Client) Cat(ctx context.Context, cid string, offset, length int64) (io.ReadCloser, error) {
	q := "?arg=" + url.QueryEscape(cid)
	if offset > 0 {
		q += "&offset=" + strconv.FormatInt(offset, 10)
	}
	if length > 0 {
		q += "&length=" + strconv.FormatInt(length, 10)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.api+"/api/v0/cat"+q, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("ipfs cat %s: %s: %s", cid, resp.Status, b)
	}
	return resp.Body, nil
}

// File is a pinned CID of known size as an io.ReadSeeker, for
// http.ServeContent: a seek only moves the offset, and the next read asks
// kubo for the bytes from there, so a Range request costs one cat of just
// that span. Close releases the read in flight.
type File struct {
	c    *Client
	ctx  context.Context
	cid  string
	size int64
	off  int64
	rc   io.ReadCloser
}

func (c *Client) Open(ctx context.Context, cid string, size int64) *File {
	return &File{c: c, ctx: ctx, cid: cid, size: size}
}

func (f *File) Read(p []byte) (int, error) {
	if f.off >= f.size {
		return 0, io.EOF
	}
	if f.rc == nil {
		rc, err := f.c.Cat(f.ctx, f.cid, f.off, f.size-f.off)
		if err != nil {
			return 0, err
		}
		f.rc = rc
	}
	n, err := f.rc.Read(p)
	f.off += int64(n)
	if err == io.EOF && f.off < f.size {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (f *File) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		offset += f.off
	case io.SeekEnd:
		offset += f.size
	}
	if offset < 0 {
		return 0, fmt.Errorf("ipfs: seek to %d", offset)
	}
	if offset != f.off {
		f.Close()
		f.off = offset
	}
	return offset, nil
}

func (f *File) Close() error {
	if f.rc == nil {
		return nil
	}
	err := f.rc.Close()
	f.rc = nil
	return err
}
