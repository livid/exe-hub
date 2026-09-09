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
	"time"
)

type Client struct {
	api    string // e.g. http://127.0.0.1:5001
	client *http.Client
}

func New(api string) *Client {
	return &Client{api: api, client: &http.Client{Timeout: 60 * time.Second}}
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

// Cat streams a pinned CID's bytes. The caller must Close the reader.
func (c *Client) Cat(cid string) (io.ReadCloser, error) {
	resp, err := c.client.Post(c.api+"/api/v0/cat?arg="+url.QueryEscape(cid), "", nil)
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
