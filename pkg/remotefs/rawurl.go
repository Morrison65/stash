package remotefs

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"strconv"
	"time"
)

// SignRawURL returns the path and query of a signed stash-fsd raw file URL
// ("/raw?p=...&z=...&e=...&s=..."), valid until expiry. zipEntry is the
// entry inside the zip file at path, or empty. See
// docs/architecture/stash-fsd.md for the signature scheme.
func SignRawURL(secret, path, zipEntry string, expiry time.Time) string {
	p := base64.RawURLEncoding.EncodeToString([]byte(path))
	z := ""
	if zipEntry != "" {
		z = base64.RawURLEncoding.EncodeToString([]byte(zipEntry))
	}
	e := strconv.FormatInt(expiry.Unix(), 10)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("raw\n" + p + "\n" + z + "\n" + e))
	s := hex.EncodeToString(mac.Sum(nil))

	q := url.Values{}
	q.Set("p", p)
	if z != "" {
		q.Set("z", z)
	}
	q.Set("e", e)
	q.Set("s", s)
	return "/raw?" + q.Encode()
}
