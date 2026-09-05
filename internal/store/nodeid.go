package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// NodeIDFile is the basename of the per-installation identity that lives
// beside the database. Every row a daemon owns is stamped with it, which is
// what lets several machines share one database without their pane ids —
// herdr's compact, recycled "1", "2", … — colliding.
const NodeIDFile = "node-id"

// nodeIDRE is the only shape a node id may take: 16 lowercase hex characters.
// Short enough to read in a listing, long enough (64 bits) that two
// installations never mint the same one by accident.
var nodeIDRE = regexp.MustCompile(`^[a-f0-9]{16}$`)

// ValidNodeID reports whether id has the shape LoadNodeID mints.
func ValidNodeID(id string) bool { return nodeIDRE.MatchString(id) }

// LoadNodeID returns this installation's node id, minting and persisting one
// in dir on first use. Creation is O_EXCL so two hap processes starting at the
// same moment on a fresh state dir converge on one id: the loser re-reads.
//
// A malformed file is an error, never silently replaced — the id is the key
// under which every row this node ever wrote is filed, and minting a new one
// would orphan all of them.
func LoadNodeID(dir string) (string, error) {
	path := filepath.Join(dir, NodeIDFile)
	if id, err := readNodeID(path); err == nil {
		return id, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint node id: %w", err)
	}
	id := hex.EncodeToString(raw[:])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create state dir for node id: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return readNodeID(path)
		}
		return "", fmt.Errorf("write node id: %w", err)
	}
	if _, err := f.WriteString(id + "\n"); err != nil {
		f.Close()
		return "", fmt.Errorf("write node id: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("write node id: %w", err)
	}
	return id, nil
}

func readNodeID(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if !ValidNodeID(id) {
		return "", fmt.Errorf("node id file %s is malformed (want 16 hex characters); "+
			"restore it from a backup rather than deleting it — every row this node wrote is filed under it", path)
	}
	return id, nil
}

// NodeBits folds a node id into the 12 bits a time-ordered id carries.
func NodeBits(id string) uint16 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return uint16(h.Sum32() & 0xFFF)
}
