package tenderduty

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/go-passwd/validator"
	"github.com/tendermint/crypto/argon2"
	"io"
	"log"
	"os"
	"runtime"
)

const (
	idTime    = 1
	idMem     = 64 * 1024 // may want to halve this and double time for smaller systems.
	idKeySize = 32        // will be doubled so there is a MAC key too....
	ivSize    = 16
	macSize   = 32
)

// getKey uses Argon2id to derive two private keys from a password. It will reject both short and commonly
// used passwords. It returns two keys and the salt it used. If no salt is provided it will create one.
func getKey(pass string, knownSalt []byte) (key, macKey, salt []byte, err error) {
	// reject really-bad passwords.
	passwordValidator := validator.New(validator.MinLength(8, nil), validator.CommonPassword(nil))
	err = passwordValidator.Validate(pass)
	if err != nil {
		return
	}

	// nil salt means we create it.
	if knownSalt == nil {
		knownSalt = make([]byte, idKeySize)
		_, err = rand.Read(knownSalt)
		if err != nil {
			return
		}
	} else if len(knownSalt) != idKeySize || bytes.Equal(knownSalt, bytes.Repeat([]byte{0}, idKeySize)) {
		err = fmt.Errorf("salt must be %d bytes and non-zero", idKeySize)
		return
	}

	// double the key size so we have a symmetric key and a mac key
	keys := argon2.IDKey([]byte(pass), knownSalt, idTime, idMem, uint8(runtime.NumCPU()), idKeySize*2)
	if len(keys) != idKeySize*2 || bytes.Equal(keys, bytes.Repeat([]byte{0}, idKeySize*2)) {
		err = errors.New("invalid key, was all zeros")
		return
	}

	return keys[:idKeySize], keys[idKeySize:], knownSalt, nil
}

// encrypt encrypts a []byte using AES256, prepends the salt and iv, appends an SHA-256 HMAC, and returns as a Base64 encoded []byte
func encrypt(plainText []byte, password string) (encryptedConfig []byte, err error) {
	if plainText == nil || len(plainText) == 0 {
		err = errors.New("invalid config file")
		return
	}

	// Get our AES256 key, mac key, and password salt:
	key, macKey, salt, err := getKey(password, nil)
	if err != nil {
		return
	}

	blk, err := aes.NewCipher(key)
	if err != nil {
		return
	}

	iv := make([]byte, ivSize)
	_, err = rand.Read(iv)
	if err != nil {
		return
	}

	// first write our salt and iv to the buffer
	buf := bytes.NewBuffer(nil)
	buf.Write(salt)
	buf.Write(iv)

	cbc := cipher.NewCBCEncrypter(blk, iv)

	// pad the plaintext
	fmt.Println("plaintext size", len(plainText))
	padLen := cbc.BlockSize() - (len(plainText) % cbc.BlockSize())
	fmt.Println("mod blocksize", padLen)
	if padLen > 0 {
		plainText = append(plainText, bytes.Repeat([]byte{uint8(padLen)}, padLen)...)
	}
	fmt.Println(len(plainText), "plaintext padded size", padLen, "added")

	// encrypt the file
	cipherText := make([]byte, len(plainText))
	cbc.CryptBlocks(cipherText, plainText)
	if cipherText == nil || len(cipherText) == 0 {
		err = errors.New("invalid ciphertext, nothing encrypted")
	}

	_, err = buf.Write(cipherText)
	if err != nil {
		return
	}
	fmt.Println("buffer length", buf.Len())

	// create an outer hmac
	signer := hmac.New(sha256.New, macKey)
	_, err = signer.Write(buf.Bytes())
	if err != nil {
		return
	}
	// sign the message
	_, err = buf.Write(signer.Sum(nil))

	encryptedConfig = make([]byte, base64.StdEncoding.EncodedLen(buf.Len()-1))
	base64.StdEncoding.Encode(encryptedConfig, buf.Bytes())
	return
}

// decrypt takes a base64 encoded []byte with salt + iv + ciphertext + mac, the password, and authenticates the HMAC
// before it gives back the decrypted configuration.
func decrypt(encodedFile []byte, password string) (plainText []byte, err error) {

	cipherText := make([]byte, base64.StdEncoding.DecodedLen(len(encodedFile)))
	size, err := base64.StdEncoding.Decode(cipherText, encodedFile)
	if err != nil {
		return
	}

	if size <= 2*idKeySize+ivSize {
		err = errors.New("ciphertext is too short")
	}

	// get our keys, salt is first idKeySize bytes
	key, macKey, _, err := getKey(password, cipherText[:idKeySize])
	if err != nil {
		return
	}
	ct := cipherText[idKeySize+ivSize : len(cipherText)-macSize-1]
	fmt.Println("authenticated length", len(cipherText[:len(cipherText)-macSize-1]))
	fmt.Println("last byte", cipherText[len(cipherText)-1])

	_ = macKey
	// authenticate
	auth := hmac.New(sha256.New, macKey)
	_, err = auth.Write(cipherText[:len(cipherText)-macSize-1])
	if err != nil {
		return
	}
	authSum := auth.Sum(nil)
	fmt.Println(hex.EncodeToString(cipherText[len(cipherText)-macSize-1 : len(cipherText)-1]))
	fmt.Println(hex.EncodeToString(authSum))
	if !bytes.Equal(cipherText[len(cipherText)-macSize-1:len(cipherText)-1], authSum) {
		err = errors.New("HMAC authentication failed")
		return
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	cbc := cipher.NewCBCDecrypter(block, cipherText[idKeySize:idKeySize+ivSize])
	plainText = make([]byte, len(ct))
	fmt.Println(len(plainText), "plaintext size")
	cbc.CryptBlocks(plainText, ct)
	if len(plainText) == 0 {
		err = errors.New("plaintext was empty")
		return
	}

	// strip padding
	if len(plainText)%block.BlockSize() != 0 {
		fmt.Println("stripping padding bytes:", len(plainText)-int(plainText[len(plainText)-1]))
		return plainText[:len(plainText)-int(plainText[len(plainText)-1])], nil
	}
	return
}

func EncryptedConfig(plaintext, ciphertext, pass string, decrypting bool) error {
	var infile, outfile = plaintext, ciphertext
	if decrypting {
		outfile, infile = plaintext, ciphertext
	}
	fin, err := os.OpenFile(infile, os.O_RDONLY, 0600)
	if err != nil {
		return err
	}
	defer fin.Close()
	inConfig, err := io.ReadAll(fin)
	if err != nil {
		return err
	}

	outConfig := make([]byte, 0)
	if decrypting {
		outConfig, err = decrypt(inConfig, pass)
		if err != nil {
			return err
		}
	} else {
		outConfig, err = encrypt(inConfig, pass)
		if err != nil {
			return err
		}
	}
	fout, err := os.OpenFile(outfile, os.O_TRUNC|os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer fout.Close()
	size, err := fout.Write(outConfig)
	if err != nil {
		return err
	}
	fileType := "encrypted"
	if decrypting {
		fileType = "decrypted"
	}
	log.Printf("wrote %d bytes to %s file %s\n", size, fileType, outfile)
	return nil
}
