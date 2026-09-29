package protocol

// UpdateSigningPayload binds an Agent download request to its HTTP host, path
// and timestamp. Both sides must use the URL host seen by the Agent.
func UpdateSigningPayload(host, path, timestamp string) []byte {
	return []byte("codegate-update-v1\nGET\n" + host + "\n" + path + "\n" + timestamp)
}
