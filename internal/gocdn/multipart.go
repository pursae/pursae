package gocdn

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// Multipart upload (CreateMultipartUpload, UploadPart, CompleteMultipartUpload,
// AbortMultipartUpload) and CopyObject. Upload picks a single PUT or a multipart upload by size, so
// a large video is sent in parts of PartSize (each signed with its own payload hash and retried on
// its own) instead of one request that has to start over on any error; a failed multipart upload
// is aborted so the bucket keeps no orphaned parts.

const (
	// MinPartSize is S3's smallest part (except the last one).
	MinPartSize = 5 << 20
	// DefaultPartSize is the part size when none is given.
	DefaultPartSize = 8 << 20
	// MaxPartSize is S3's largest multipart part.
	MaxPartSize = 5 << 30
	maxParts    = 10000
	// MaxObjectSize is the largest object S3 can assemble from multipart parts.
	MaxObjectSize = maxParts * MaxPartSize
)

// Part is an uploaded part.
type Part struct {
	Number int
	ETag   string
}

// UploadOptions decide between a single PUT and a multipart upload.
type UploadOptions struct {
	PutOptions
	Threshold int64 // multipart from this size on (0: 16 MB)
	PartSize  int64 // 0: DefaultPartSize; raised when the object would need more than 10 000 parts
	Retries   int   // per part (0: 3)
}

// Upload stores size bytes read from r under key: one PUT below the threshold, a multipart upload
// from it on. It reports whether multipart was used.
func (c *Client) Upload(ctx context.Context, key string, r io.ReaderAt, size int64, o UploadOptions) (bool, error) {
	if size < 0 {
		return false, errors.New("upload size must not be negative")
	}
	if size > MaxObjectSize {
		return false, fmt.Errorf("upload size exceeds S3's %d-byte object limit", MaxObjectSize)
	}
	threshold := o.Threshold
	if threshold <= 0 {
		threshold = 16 << 20
	}
	if size < threshold {
		buf := make([]byte, size)
		if n, err := r.ReadAt(buf, 0); int64(n) < size {
			return false, fmt.Errorf("reading the file: %w", err)
		}
		return false, c.Put(ctx, key, buf, o.PutOptions)
	}
	part := o.PartSize
	if part <= 0 {
		part = DefaultPartSize
	}
	if part < MinPartSize {
		part = MinPartSize
	}
	if part > MaxPartSize {
		part = MaxPartSize
	}
	if part > size {
		part = size
	}
	requiredPart := (size-1)/maxParts + 1
	if part < requiredPart {
		part = requiredPart
	}
	retries := o.Retries
	if retries <= 0 {
		retries = 3
	}
	id, err := c.CreateMultipartUpload(ctx, key, o.PutOptions)
	if err != nil {
		return true, err
	}
	var parts []Part
	buf := make([]byte, part)
	for off, n := int64(0), 1; off < size; off, n = off+part, n+1 {
		l := min(part, size-off)
		chunk := buf[:l]
		if got, err := r.ReadAt(chunk, off); int64(got) < l {
			_ = c.AbortMultipartUpload(context.WithoutCancel(ctx), key, id)
			return true, fmt.Errorf("reading the file: %w", err)
		}
		var etag string
		for try := 0; ; try++ {
			etag, err = c.UploadPart(ctx, key, id, n, chunk)
			if err == nil || try+1 >= retries || ctx.Err() != nil {
				break
			}
		}
		if err != nil {
			_ = c.AbortMultipartUpload(context.WithoutCancel(ctx), key, id)
			return true, fmt.Errorf("part %d: %w", n, err)
		}
		parts = append(parts, Part{Number: n, ETag: etag})
	}
	if err := c.CompleteMultipartUpload(ctx, key, id, parts); err != nil {
		_ = c.AbortMultipartUpload(context.WithoutCancel(ctx), key, id)
		return true, err
	}
	return true, nil
}

// CreateMultipartUpload starts a multipart upload; the options apply to the final object.
func (c *Client) CreateMultipartUpload(ctx context.Context, key string, o PutOptions) (string, error) {
	resp, err := c.do(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, []byte{}, c.withSSEC(o.headers()))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var x struct {
		XMLName  xml.Name
		UploadID string `xml:"UploadId"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&x); err != nil {
		return "", fmt.Errorf("CreateMultipartUpload: decoding response: %w", err)
	}
	if x.XMLName.Local != "InitiateMultipartUploadResult" {
		return "", fmt.Errorf("CreateMultipartUpload: unexpected response root %q", x.XMLName.Local)
	}
	if x.UploadID == "" {
		return "", errors.New("CreateMultipartUpload: no upload ID")
	}
	return x.UploadID, nil
}

// UploadPart sends part n (1-based) and returns its ETag.
func (c *Client) UploadPart(ctx context.Context, key, uploadID string, n int, data []byte) (string, error) {
	if n < 1 || n > maxParts {
		return "", fmt.Errorf("UploadPart: part number must be from 1 through %d", maxParts)
	}
	q := url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {uploadID}}
	resp, err := c.do(ctx, http.MethodPut, key, q, data, c.withSSEC(nil))
	if err != nil {
		return "", err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", errors.New("UploadPart: no ETag in the answer")
	}
	return etag, nil
}

// CompleteMultipartUpload assembles the parts. S3 may answer 200 with an error in the body.
func (c *Client) CompleteMultipartUpload(ctx context.Context, key, uploadID string, parts []Part) error {
	var b bytes.Buffer
	b.WriteString(`<CompleteMultipartUpload xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	for _, p := range parts {
		b.WriteString("<Part><PartNumber>" + strconv.Itoa(p.Number) + "</PartNumber><ETag>")
		_ = xml.EscapeText(&b, []byte(p.ETag))
		b.WriteString("</ETag></Part>")
	}
	b.WriteString(`</CompleteMultipartUpload>`)
	h := http.Header{}
	h.Set("Content-Type", "application/xml")
	resp, err := c.do(ctx, http.MethodPost, key, url.Values{"uploadId": {uploadID}}, b.Bytes(), h)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return errorInBody(resp)
}

// AbortMultipartUpload discards an unfinished upload and its parts.
func (c *Client) AbortMultipartUpload(ctx context.Context, key, uploadID string) error {
	resp, err := c.do(ctx, http.MethodDelete, key, url.Values{"uploadId": {uploadID}}, nil, nil)
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// Copy is CopyObject: srcBucket/srcKey (same endpoint and credentials) to key in this client's
// bucket, with the options' headers (metadata directive REPLACE). Objects up to 5 GB.
func (c *Client) Copy(ctx context.Context, srcBucket, srcKey, key string, o PutOptions) error {
	h := c.withSSEC(o.headers())
	if len(o.CopySourceSSECKey) != 0 {
		sseCHeaders(h, sseCCopyPrefix, o.CopySourceSSECKey)
	}
	h.Set("X-Amz-Copy-Source", EscapePath("/"+srcBucket+"/"+srcKey))
	h.Set("X-Amz-Metadata-Directive", "REPLACE")
	resp, err := c.do(ctx, http.MethodPut, key, nil, []byte{}, h)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return errorInBody(resp)
}

// errorInBody reads a 200 answer that may still carry an <Error> (CompleteMultipartUpload,
// CopyObject).
func errorInBody(resp *http.Response) error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var x struct {
		XMLName xml.Name
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if err := xml.Unmarshal(data, &x); err != nil {
		return fmt.Errorf("S3 response: %w", err)
	}
	if x.XMLName.Local != "Error" {
		return nil
	}
	return &Error{Status: resp.StatusCode, Code: x.Code, Message: x.Message}
}
