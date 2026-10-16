package githubapp

import (
	"fmt"
	"strings"
)

// ResolveAccount is shared by policy, the minter and the mapping display.
// Each entry represents one installation, including duplicate accounts: a
// duplicate must be reported as ambiguous, never silently deduplicated.
func ResolveAccount(accounts []string, owner string) (int, error) {
	found := -1
	for i, account := range accounts {
		if owner != "" && !strings.EqualFold(account, owner) {
			continue
		}
		if found >= 0 {
			return -1, fmt.Errorf("github route ambiguous: identify one account with one installation")
		}
		found = i
	}
	if found < 0 {
		return -1, fmt.Errorf("github route missing: no connected installation for this account")
	}
	return found, nil
}
