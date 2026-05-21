// Copyright 2026 Zibby Lab. Apache-2.0.

// Package node holds the daemon's stable identity and role.
//
// In v0.1 every daemon is a "solo" node — single-machine MVP. The shape is
// intentionally future-compatible with a multi-node cluster (pilot/worker/Raft):
// the same ID + Role + Heartbeat surface that solo uses will plug into the
// cluster package when it lands.
package node

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Role identifies what this node does within its (possibly degenerate) cluster.
// In v0.1 the only valid role is RoleSolo. RolePilot / RoleWorker are reserved
// so consumers can switch on Role without hitting an unknown variant when
// clustering arrives.
type Role string

const (
	RoleSolo   Role = "solo"
	RolePilot  Role = "pilot"
	RoleWorker Role = "worker"
)

// ID is the stable identifier persisted in the state directory across restarts.
type ID string

// Node is the daemon's identity surface. Implementations must be safe for
// concurrent reads — Heartbeat may be called from multiple goroutines.
type Node interface {
	ID() ID
	Role() Role
	StartedAt() time.Time
	Heartbeat() time.Time
	Touch()
}

// Solo is the single-node implementation used by v0.1.
type Solo struct {
	id        ID
	startedAt time.Time

	mu        sync.RWMutex
	heartbeat time.Time
}

// LoadOrInit returns the persistent Node identity, creating it on first run.
// The id is written to <stateDir>/node.id as a 32-hex-char string.
//
// Returning a non-Solo Role from this constructor is the seam where cluster
// support will land: a future Bootstrap() would consult the cluster package
// to elect pilot/worker, then construct a different Node implementation.
func LoadOrInit(stateDir string) (*Solo, error) {
	if stateDir == "" {
		return nil, errors.New("node.LoadOrInit: stateDir required")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("node: ensure state dir: %w", err)
	}
	path := filepath.Join(stateDir, "node.id")
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		id := strings.TrimSpace(string(raw))
		if !validID(id) {
			return nil, fmt.Errorf("node: corrupt id at %s", path)
		}
		return newSolo(ID(id)), nil
	case errors.Is(err, os.ErrNotExist):
		id, err := generateID()
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(id), 0o600); err != nil {
			return nil, fmt.Errorf("node: persist id: %w", err)
		}
		return newSolo(ID(id)), nil
	default:
		return nil, fmt.Errorf("node: read id: %w", err)
	}
}

func newSolo(id ID) *Solo {
	now := time.Now().UTC()
	return &Solo{id: id, startedAt: now, heartbeat: now}
}

func (s *Solo) ID() ID         { return s.id }
func (s *Solo) Role() Role     { return RoleSolo }
func (s *Solo) StartedAt() time.Time { return s.startedAt }

func (s *Solo) Heartbeat() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.heartbeat
}

// Touch is called by the daemon's heartbeat goroutine to record liveness.
// In clustered mode this will also push to the consensus log.
func (s *Solo) Touch() {
	now := time.Now().UTC()
	s.mu.Lock()
	s.heartbeat = now
	s.mu.Unlock()
}

func generateID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("node: rand: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func validID(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
