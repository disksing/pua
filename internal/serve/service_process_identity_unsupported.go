//go:build !darwin && !linux

package serve

func readServiceProcessIdentity(pid int) (serviceProcessIdentity, error) {
	return serviceProcessIdentity{}, errProcessIdentityUnavailable
}

func platformServiceProcessIdentityMatches(identity serviceProcessIdentity, startID, token, digest string) bool {
	return false
}

func serviceProcessIdentityRequired() bool { return false }
