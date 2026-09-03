package domain

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

const (
	RunKindDemo        = "DEMO"
	RunStatusSucceeded = "SUCCEEDED"
)

// Run is the minimal M1 history record. It intentionally models demo work only.
type Run struct {
	ID          string
	Kind        string
	Status      string
	Summary     string
	RequestedAt time.Time
	StartedAt   time.Time
	FinishedAt  time.Time
}

// NewID returns a random UUID suitable for durable M1 identifiers.
func NewID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate UUID: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80

	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}
