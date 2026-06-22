package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// SignatureHeader carries the HMAC-SHA256 signature of the request body.
const SignatureHeader = "X-Forge-Signature"

// EventHeader carries the event type, so a receiver can route without parsing
// the body.
const EventHeader = "X-Forge-Event"

// Sign returns the GitHub-style "sha256=<hex>" HMAC-SHA256 of body keyed by
// secret. A receiver recomputes this over the raw body and compares with
// hmac.Equal to authenticate the delivery.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
