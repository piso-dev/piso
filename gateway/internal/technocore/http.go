package technocore

import (
	"net/http"
	"time"
)

// serverHTTP bounds every coordinator call. http.DefaultClient has no
// timeout; a hung server used to stall the websocket read loop inside
// syncSecrets, so pongs stopped and the session sat until TCP died.
var serverHTTP = &http.Client{Timeout: 15 * time.Second}
