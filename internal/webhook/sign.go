package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

// SignatureHeader carries the HMAC-SHA256 signature of "{timestamp}.{body}".
const SignatureHeader = "X-Forge-Signature"

// EventHeader carries the event type, so a receiver can route without parsing
// the body.
const EventHeader = "X-Forge-Event"

// DeliveryHeader carries a stable per-delivery id (same across retries), so a
// receiver can dedup deliveries it has already processed.
const DeliveryHeader = "X-Forge-Delivery"

// TimestampHeader carries the Unix-seconds timestamp that was signed alongside
// the body. A receiver rejects deliveries whose timestamp is outside its
// tolerance window (replay protection).
const TimestampHeader = "X-Forge-Timestamp"

// Sign returns the GitHub-style "sha256=<hex>" HMAC-SHA256 over the signed
// payload "{timestamp}.{body}" (Stripe-style). Binding the timestamp into the
// MAC lets a receiver reject replays: it recomputes this over the raw body plus
// the X-Forge-Timestamp value and compares with hmac.Equal (see Verify).
func Sign(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify authenticates a delivery for a receiver: it rejects timestamps outside
// tolerance of now (replay protection, when tolerance > 0), then recomputes the
// signature over "{timestamp}.{body}" and compares in constant time. This is
// the reference verification a subscriber should perform.
func Verify(secret, signature string, timestamp int64, body []byte, tolerance time.Duration, now time.Time) bool {
	if tolerance > 0 {
		skew := now.Unix() - timestamp
		if skew < 0 {
			skew = -skew
		}
		if time.Duration(skew)*time.Second > tolerance {
			return false
		}
	}
	expected := Sign(secret, timestamp, body)
	return hmac.Equal([]byte(signature), []byte(expected))
}
