//go:build !windows

package main

import "errors"

func dpapiSeal([]byte) (string, error) {
	return "", errors.New("dpapi requires Windows")
}

func dpapiOpen(string) ([]byte, error) {
	return nil, errors.New("dpapi requires Windows")
}
