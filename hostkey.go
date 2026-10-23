package main

import "golang.org/x/crypto/ssh"

func hostKeyFingerprint(authorizedKey string) string {
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorizedKey))
	if err != nil {
		return "invalid"
	}
	return ssh.FingerprintSHA256(k)
}
