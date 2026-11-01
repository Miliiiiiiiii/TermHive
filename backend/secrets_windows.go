//go:build windows

package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	crypt32                = windows.NewLazySystemDLL("crypt32.dll")
	procCryptProtectData   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
)

const cryptProtectUIForbidden = 0x1

// dpapiSeal encrypts with CryptProtectData: the ciphertext can only be
// opened by the same Windows user account, so stealing specs.json alone is
// not enough to recover the stored credentials.
func dpapiSeal(plaintext []byte) (string, error) {
	if len(plaintext) == 0 {
		return "", errors.New("dpapi: empty plaintext")
	}
	description, err := windows.UTF16PtrFromString("termhive credentials")
	if err != nil {
		return "", err
	}
	in := windows.BLOB{Size: uint32(len(plaintext)), BlobData: &plaintext[0]}
	var out windows.BLOB
	r1, _, callErr := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)),
		uintptr(unsafe.Pointer(description)),
		0, 0, 0,
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	if r1 == 0 {
		return "", fmt.Errorf("CryptProtectData: %w", callErr)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.BlobData)))
	return base64.StdEncoding.EncodeToString(unsafe.Slice(out.BlobData, out.Size)), nil
}

func dpapiOpen(data string) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) == 0 {
		return nil, errors.New("dpapi: empty payload")
	}
	in := windows.BLOB{Size: uint32(len(ciphertext)), BlobData: &ciphertext[0]}
	var out windows.BLOB
	r1, _, callErr := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // description out, ignored
		0, 0, 0,
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	if r1 == 0 {
		return nil, fmt.Errorf("CryptUnprotectData (数据可能由其他 Windows 账户/机器加密): %w", callErr)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.BlobData)))
	return append([]byte(nil), unsafe.Slice(out.BlobData, out.Size)...), nil
}
