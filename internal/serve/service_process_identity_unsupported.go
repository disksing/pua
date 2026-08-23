//go:build !darwin && !linux

package serve

func readServiceProcessIdentity(pid int) (serviceProcessIdentity, error) {
	return serviceProcessIdentity{}, errProcessIdentityUnavailable
}

func readServiceProcessGroupMembers(processGroup int) ([]serviceProcessIdentity, error) {
	return nil, errProcessIdentityUnavailable
}

func platformServiceProcessIdentityMatches(identity serviceProcessIdentity, startID, token, digest string) bool {
	return false
}

func serviceProcessIdentityInspectionAvailable() bool { return false }

func serviceProcessIdentityMarkerRequired() bool { return false }

func platformServiceProcessGroupMemberMatches(identity serviceProcessIdentity, token, markerPath string) (bool, error) {
	return false, errProcessIdentityUnavailable
}
