package qoder

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" // Qoder's wire protocol requires MD5 signatures.
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
)

const ServerPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

const customAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
const stdAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

func EncodePayload(value any) (string, error) {
	var raw []byte
	var err error
	if text, ok := value.(string); ok {
		raw = []byte(text)
	} else {
		raw, err = json.Marshal(value)
	}
	if err != nil {
		return "", err
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	a := len(encoded) / 3
	rearranged := encoded[len(encoded)-a:] + encoded[a:len(encoded)-a] + encoded[:a]
	translation := make(map[byte]byte, 65)
	for i := range stdAlphabet {
		translation[stdAlphabet[i]] = customAlphabet[i]
	}
	translation['='] = '$'
	out := make([]byte, len(rearranged))
	for i := range rearranged {
		mapped, ok := translation[rearranged[i]]
		if !ok {
			return "", errors.New("qoder payload contains an unsupported character")
		}
		out[i] = mapped
	}
	return string(out), nil
}

func MD5Hex(value string) string { sum := md5.Sum([]byte(value)); return hex.EncodeToString(sum[:]) }

func EncryptSession(identity any) (encryptedKey, encryptedInfo string, err error) {
	key := make([]byte, 16)
	if _, err = rand.Read(key); err != nil {
		return "", "", err
	}
	block, _ := pem.Decode([]byte(ServerPublicKeyPEM))
	if block == nil {
		return "", "", errors.New("invalid Qoder server public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", "", err
	}
	publicKey, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return "", "", errors.New("Qoder server key is not RSA")
	}
	wrapped, err := rsa.EncryptPKCS1v15(rand.Reader, publicKey, key)
	if err != nil {
		return "", "", err
	}
	plain, err := json.Marshal(identity)
	if err != nil {
		return "", "", err
	}
	cipherBlock, err := aes.NewCipher(key)
	if err != nil {
		return "", "", err
	}
	padding := aes.BlockSize - len(plain)%aes.BlockSize
	for i := 0; i < padding; i++ {
		plain = append(plain, byte(padding))
	}
	encrypted := make([]byte, len(plain))
	cipher.NewCBCEncrypter(cipherBlock, key).CryptBlocks(encrypted, plain)
	return base64.StdEncoding.EncodeToString(wrapped), base64.StdEncoding.EncodeToString(encrypted), nil
}
