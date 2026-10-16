package telegram

import (
	"piso/gateway/internal/model"
	"piso/gateway/internal/store"
)

// Secret returns the stored bot-token secret, if one exists.
//
// The token is a gateway-only secret with Kind "telegram": it has no
// placeholder, no EnvKey and no substitution rule, so it can never be exported
// to a worker or replayed into a worker request. Callers must not put its Value
// in a response body.
func Secret(recs []store.SecretRec) (store.SecretRec, bool) {
	for _, rec := range recs {
		if rec.Kind == model.SecretKindTelegram {
			return rec, true
		}
	}
	return store.SecretRec{}, false
}
