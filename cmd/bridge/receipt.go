package main

import (
	"errors"
	"io"
	"os"

	bridge "gitea-topic-bridge"
)

func verifyReceiptFile(path, trustedKey string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	const limit = 16 << 20
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return err
	}
	if len(data) > limit {
		return errors.New("receipt exceeds 16 MiB")
	}
	return bridge.VerifyReceipt(data, trustedKey)
}
