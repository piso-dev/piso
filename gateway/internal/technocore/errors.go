package technocore

import "errors"

// ErrAlreadyPaired is returned when a pairing action is attempted on an
// enrolled gateway. Re-enrolling means clearing identity.json from the CLI;
// the dashboard stays read-only once a gateway is live.
var ErrAlreadyPaired = errors.New("this gateway is already paired")
