package auth

import "crypto/rand"

// fillRandom 填充密码学安全随机字节。
func fillRandom(b []byte) error {
	_, err := rand.Read(b)
	return err
}
