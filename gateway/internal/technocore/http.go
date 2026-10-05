package technocore

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	tc "piso/internal/technocore"
)

// serverHTTP bounds every coordinator call. http.DefaultClient has no
// timeout; a hung server used to stall the websocket read loop inside
// syncSecrets, so pongs stopped and the session sat until TCP died.
var serverHTTP = &http.Client{Timeout: 15 * time.Second}

// ErrUnpaired means the gateway has no active coordinator link.
var ErrUnpaired = fmt.Errorf("unpaired")

// Call is a signed JSON request to the coordinator. Query is not signed.
func Call(id tc.Identity, method, path string, query url.Values, body []byte) (int, []byte, error) {
	if id.Status != tc.StatusActive || strings.TrimSpace(id.ServerURL) == "" {
		return 0, nil, ErrUnpaired
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u, err := url.Parse(strings.TrimRight(id.ServerURL, "/") + path)
	if err != nil {
		return 0, nil, err
	}
	if query != nil {
		u.RawQuery = query.Encode()
	}
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, u.String(), rdr)
	if err != nil {
		return 0, nil, err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := tc.SignRequest(req, id, body); err != nil {
		return 0, nil, err
	}
	res, err := serverHTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return res.StatusCode, raw, nil
}
