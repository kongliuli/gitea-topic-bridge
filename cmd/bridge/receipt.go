package main

import (
	"errors"
	"io"
	"os"

	bridge "gitea-topic-bridge"
)

func readBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("receipt or proof exceeds its size limit")
	}
	return data, nil
}

func verifyReceiptFile(path, trustedKey, proofPath, trustedAnchorKey string) error {
	if (proofPath == "") != (trustedAnchorKey == "") {
		return errors.New("anchor proof and trusted anchor key must be supplied together")
	}
	data, err := readBounded(path, 16<<20)
	if err != nil {
		return err
	}
	if proofPath != "" {
		proof, err := readBounded(proofPath, 4096)
		if err != nil {
			return err
		}
		return bridge.VerifyReceiptAnchor(data, proof, trustedKey, trustedAnchorKey)
	}
	return bridge.VerifyReceipt(data, trustedKey)
}
