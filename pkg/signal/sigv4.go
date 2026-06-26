package signal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

// AWSCredentials are the SigV4 signing credentials. SessionToken is set for
// temporary (STS) credentials and is empty for long-lived keys.
type AWSCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// signV4 signs an HTTP request with AWS Signature Version 4 and sets the
// Authorization, X-Amz-Date, X-Amz-Content-Sha256, and (when present)
// X-Amz-Security-Token headers. payload is the exact request body. region and
// service scope the signature. It is hand-rolled so the open core takes no AWS
// SDK dependency.
func signV4(req *http.Request, payload []byte, creds AWSCredentials, region, service string, t time.Time) {
	t = t.UTC()
	amzDate := t.Format("20060102T150405Z")
	dateStamp := t.Format("20060102")
	payloadHash := hexHash(payload)

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}

	// Sign host, content-type, and every x-amz-* header.
	headers := map[string]string{"host": req.URL.Host}
	if ct := req.Header.Get("Content-Type"); ct != "" {
		headers["content-type"] = ct
	}
	for name := range req.Header {
		if ln := strings.ToLower(name); strings.HasPrefix(ln, "x-amz-") {
			headers[ln] = strings.TrimSpace(req.Header.Get(name))
		}
	}
	names := make([]string, 0, len(headers))
	for n := range headers {
		names = append(names, n)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n)
		canonHeaders.WriteByte(':')
		canonHeaders.WriteString(headers[n])
		canonHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(names, ";")

	uri := req.URL.EscapedPath()
	if uri == "" {
		uri = "/"
	}

	req.Header.Set("Authorization", sigV4Authorization(
		req.Method, uri, req.URL.RawQuery, canonHeaders.String(), signedHeaders, payloadHash,
		creds, region, service, amzDate, dateStamp,
	))
}

// sigV4Authorization builds the Authorization header value from fully-prepared
// canonical components. It is split out from signV4 so the algorithm can be
// tested directly against AWS's published SigV4 test vectors.
func sigV4Authorization(method, canonicalURI, canonicalQuery, canonicalHeaders, signedHeaders, payloadHash string, creds AWSCredentials, region, service, amzDate, dateStamp string) string {
	canonicalRequest := method + "\n" + canonicalURI + "\n" + canonicalQuery + "\n" +
		canonicalHeaders + "\n" + signedHeaders + "\n" + payloadHash
	scope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hexHash([]byte(canonicalRequest))
	signature := hex.EncodeToString(hmacSHA256(signingKey(creds.SecretAccessKey, dateStamp, region, service), []byte(stringToSign)))
	return "AWS4-HMAC-SHA256 Credential=" + creds.AccessKeyID + "/" + scope +
		", SignedHeaders=" + signedHeaders + ", Signature=" + signature
}

// signingKey derives the SigV4 signing key from the secret and the request scope.
func signingKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func hexHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
