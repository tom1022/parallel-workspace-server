package evacuation

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// signedHeaders is the fixed set this client signs. Keeping it constant rather
// than deriving it from the request keeps the canonical form trivially
// auditable; every request here carries exactly these three.
const signedHeaders = "host;x-amz-content-sha256;x-amz-date"

// S3 is the object store side of the evacuation destination (Garage, S3 API).
// SigV4 is signed here rather than through an SDK so the supervisor keeps a
// dependency-free go.mod and the workspace image stays small.
type S3 struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	Client    *http.Client

	// now is a seam for the signing test's frozen clock.
	now func() time.Time
}

func (c *S3) Validate() error {
	switch {
	case c.Endpoint == "":
		return fmt.Errorf("evacuation: object store endpoint is not configured")
	case c.Bucket == "":
		return fmt.Errorf("evacuation: object store bucket is not configured")
	case c.AccessKey == "":
		return fmt.Errorf("evacuation: object store access key is not configured")
	case c.SecretKey == "":
		return fmt.Errorf("evacuation: object store secret key is not configured")
	}
	return nil
}

// PutFile uploads path to key, overwriting whatever is there. It returns the
// number of bytes uploaded. The file is read twice — once to hash it, once to
// send it — because SigV4 needs the payload digest up front and buffering a
// whole bundle in memory would not be worth avoiding that.
func (c *S3) PutFile(ctx context.Context, key, path string) (int64, error) {
	sum, size, err := hashFile(path)
	if err != nil {
		return 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	req, err := c.newRequest(ctx, http.MethodPut, key, f)
	if err != nil {
		return 0, err
	}
	req.ContentLength = size
	c.sign(req, sum)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, fmt.Errorf("evacuation: put %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("evacuation: put %s: %s: %s", key, resp.Status, strings.TrimSpace(string(body)))
	}
	return size, nil
}

// GetFile downloads key into path, returning the number of bytes written. A
// missing key yields an empty file and a zero size, which restore reads the
// same way it reads a zero-length object: nothing local to replay.
func (c *S3) GetFile(ctx context.Context, key, path string) (int64, error) {
	req, err := c.newRequest(ctx, http.MethodGet, key, nil)
	if err != nil {
		return 0, err
	}
	c.sign(req, emptyPayloadSHA)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, fmt.Errorf("evacuation: get %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return 0, os.WriteFile(path, nil, 0o600)
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("evacuation: get %s: %s: %s", key, resp.Status, strings.TrimSpace(string(body)))
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return io.Copy(f, resp.Body)
}

// Head reports whether key exists.
func (c *S3) Head(ctx context.Context, key string) (bool, error) {
	req, err := c.newRequest(ctx, http.MethodHead, key, nil)
	if err != nil {
		return false, err
	}
	c.sign(req, emptyPayloadSHA)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return false, fmt.Errorf("evacuation: head %s: %w", key, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode >= 300:
		return false, fmt.Errorf("evacuation: head %s: %s", key, resp.Status)
	}
	return true, nil
}

func (c *S3) newRequest(ctx context.Context, method, key string, body io.Reader) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.Endpoint, "/")+"/"+c.Bucket+"/"+key, body)
}

// sign adds the SigV4 headers. Only the three headers in signedHeaders are
// covered, so nothing else on the request may be relied on by the peer.
func (c *S3) sign(req *http.Request, payloadSHA string) {
	t := c.clock()
	stamp := t.Format("20060102T150405Z")
	date := stamp[:8]

	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payloadSHA)

	canonical := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		"host:" + req.URL.Host + "\n" +
			"x-amz-content-sha256:" + payloadSHA + "\n" +
			"x-amz-date:" + stamp + "\n",
		signedHeaders,
		payloadSHA,
	}, "\n")

	scope := date + "/" + c.Region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		stamp,
		scope,
		sha256Hex([]byte(canonical)),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+c.SecretKey), date)
	key = hmacSHA256(key, c.Region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.AccessKey, scope, signedHeaders, hex.EncodeToString(hmacSHA256(key, stringToSign)),
	))
}

func (c *S3) clock() time.Time {
	if c.now != nil {
		return c.now().UTC()
	}
	return time.Now().UTC()
}

func (c *S3) httpClient() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

// emptyPayloadSHA is sha256 of the empty string, the digest SigV4 requires for
// a body-less request.
const emptyPayloadSHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
