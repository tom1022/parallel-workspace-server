package evacuation

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// referenceSignature was produced independently by botocore's SigV4Auth for
// the exact request this test builds, so a canonicalization mistake here
// cannot agree with itself and pass.
const (
	referenceKey        = "workspace/ws-abc/latest/bundle.git"
	referenceBody       = "hello bundle"
	referencePayloadSHA = "04cfecf64270c52b81da10bf6890b24fa73ee79715c44d1bc443dd9dd1de04d0"
	referenceAuth       = "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20260905/garage/s3/aws4_request, " +
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date, " +
		"Signature=875326684fce30d8619abeb9559e3f0dbe8dfb16bc361a2a54de4e9fcb845467"
)

func referenceStore(endpoint string) *S3 {
	return &S3{
		Endpoint:  endpoint,
		Bucket:    "workspace",
		Region:    "garage",
		AccessKey: "AKIAIOSFODNN7EXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		now:       func() time.Time { return time.Date(2026, 9, 5, 12, 34, 56, 0, time.UTC) },
	}
}

func TestSignMatchesReferenceVector(t *testing.T) {
	store := referenceStore("http://garage.garage.svc.cluster.local:3900")
	req, err := store.newRequest(context.Background(), http.MethodPut, referenceKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	store.sign(req, referencePayloadSHA)

	if got := req.Header.Get("Authorization"); got != referenceAuth {
		t.Errorf("Authorization\n got: %s\nwant: %s", got, referenceAuth)
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20260905T123456Z" {
		t.Errorf("X-Amz-Date = %q", got)
	}
	if got := req.Header.Get("X-Amz-Content-Sha256"); got != referencePayloadSHA {
		t.Errorf("X-Amz-Content-Sha256 = %q", got)
	}
	if got := req.URL.Path; got != "/workspace/"+referenceKey {
		t.Errorf("path = %q", got)
	}
}

func TestPutFileUploadsSignedBody(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotSHA, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotMethod, gotPath, gotBody = r.Method, r.URL.Path, string(body)
		gotAuth, gotSHA = r.Header.Get("Authorization"), r.Header.Get("X-Amz-Content-Sha256")
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "bundle.git")
	if err := os.WriteFile(path, []byte(referenceBody), 0o600); err != nil {
		t.Fatal(err)
	}

	store := referenceStore(srv.URL)
	n, err := store.PutFile(context.Background(), referenceKey, path)
	if err != nil {
		t.Fatal(err)
	}

	if n != int64(len(referenceBody)) {
		t.Errorf("size = %d, want %d", n, len(referenceBody))
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %q", gotMethod)
	}
	if gotPath != "/workspace/"+referenceKey {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody != referenceBody {
		t.Errorf("body = %q", gotBody)
	}
	if gotSHA != referencePayloadSHA {
		t.Errorf("content sha = %q", gotSHA)
	}
	if gotAuth == "" {
		t.Error("request went out unsigned")
	}
}

func TestPutFileReportsServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "AccessDenied", http.StatusForbidden)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "bundle.git")
	if err := os.WriteFile(path, []byte(referenceBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := referenceStore(srv.URL).PutFile(context.Background(), referenceKey, path); err == nil {
		t.Fatal("expected an error for a 403 response")
	}
}

func TestHeadReportsPresence(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %q", r.Method)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	store := referenceStore(srv.URL)

	ok, err := store.Head(context.Background(), referenceKey)
	if err != nil || !ok {
		t.Fatalf("200: ok=%v err=%v", ok, err)
	}

	status = http.StatusNotFound
	ok, err = store.Head(context.Background(), referenceKey)
	if err != nil || ok {
		t.Fatalf("404: ok=%v err=%v", ok, err)
	}

	status = http.StatusInternalServerError
	if _, err := store.Head(context.Background(), referenceKey); err == nil {
		t.Fatal("expected an error for a 500 response")
	}
}

func TestStoreRequiresFullConfiguration(t *testing.T) {
	if err := (&S3{Bucket: "workspace", AccessKey: "a", SecretKey: "b"}).Validate(); err == nil {
		t.Error("missing endpoint accepted")
	}
	if err := (&S3{Endpoint: "http://x", AccessKey: "a", SecretKey: "b"}).Validate(); err == nil {
		t.Error("missing bucket accepted")
	}
	if err := (&S3{Endpoint: "http://x", Bucket: "workspace", SecretKey: "b"}).Validate(); err == nil {
		t.Error("missing access key accepted")
	}
	if err := (&S3{Endpoint: "http://x", Bucket: "workspace", AccessKey: "a"}).Validate(); err == nil {
		t.Error("missing secret key accepted")
	}
	if err := (&S3{Endpoint: "http://x", Bucket: "workspace", AccessKey: "a", SecretKey: "b"}).Validate(); err != nil {
		t.Errorf("complete configuration rejected: %v", err)
	}
}

func TestGetFileDownloadsToPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q", r.Method)
		}
		if r.Header.Get("Authorization") == "" {
			t.Error("request was not signed")
		}
		_, _ = w.Write([]byte(referenceBody))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "bundle.git")
	size, err := referenceStore(srv.URL).GetFile(context.Background(), referenceKey, path)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(referenceBody)) {
		t.Errorf("size = %d, want %d", size, len(referenceBody))
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != referenceBody {
		t.Errorf("body = %q", got)
	}
}

// A key that was never written is not an error: restore treats it the same as
// an empty object — there is simply nothing local to replay.
func TestGetFileReportsMissingKeyAsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such key", http.StatusNotFound)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "bundle.git")
	size, err := referenceStore(srv.URL).GetFile(context.Background(), referenceKey, path)
	if err != nil {
		t.Fatal(err)
	}
	if size != 0 {
		t.Errorf("size = %d, want 0", size)
	}
}
