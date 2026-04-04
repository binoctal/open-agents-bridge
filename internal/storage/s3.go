package storage

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/open-agents/open-agents-bridge/internal/config"
)

type S3Uploader struct {
	config *config.S3Config
}

func NewS3Uploader(cfg *config.S3Config) *S3Uploader {
	return &S3Uploader{config: cfg}
}

func (u *S3Uploader) Upload(key string, data []byte) error {
	if u.config == nil || u.config.Bucket == "" {
		return fmt.Errorf("S3 not configured")
	}

	endpoint := u.config.Endpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://%s.s3.%s.amazonaws.com", u.config.Bucket, u.config.Region)
	}

	url := fmt.Sprintf("%s/%s", endpoint, key)
	req, err := http.NewRequest("PUT", url, bytes.NewReader(data))
	if err != nil {
		return err
	}

	u.signRequest(req, data)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("S3 upload failed: %s - %s", resp.Status, string(body))
	}
	return nil
}

func (u *S3Uploader) signRequest(req *http.Request, payload []byte) {
	now := time.Now().UTC()
	dateStamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", sha256Hex(payload))
	req.Header.Set("Content-Type", "application/json")

	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, u.config.Region)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s", amzDate, credentialScope, sha256Hex([]byte(req.URL.Path)))
	signingKey := getSignatureKey(u.config.SecretKey, dateStamp, u.config.Region, "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=%s",
		u.config.AccessKey, credentialScope, signature))
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func getSignatureKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}
