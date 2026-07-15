package store

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// CheckpointRow is the full row model for journal_checkpoints used by chain
// logic, sealing, and insert. The slim Checkpoint is the stats projection only.
type CheckpointRow struct {
	Seq          int64
	IDFrom       int64
	IDTo         int64
	Digest       []byte
	ParentDigest []byte
	Signature    []byte
	Location     string
	RecordedAt   string
}

// ComputeDigest is the pure unit hash over compacted rows (or their byte
// representations) in id order. Uses stdlib sha256. This is the digest stored
// in a checkpoint.
func ComputeDigest(compacted [][]byte) []byte {
	h := sha256.New()
	for _, p := range compacted {
		var lbuf [8]byte
		binary.BigEndian.PutUint64(lbuf[:], uint64(len(p)))
		h.Write(lbuf[:])
		h.Write(p)
	}
	return h.Sum(nil)
}

// ParentFor returns the parent_digest value to use for a new checkpoint
// following prev (nil for the first checkpoint in the chain).
func ParentFor(prev *CheckpointRow) []byte {
	if prev == nil || len(prev.Digest) == 0 {
		return nil
	}
	d := make([]byte, len(prev.Digest))
	copy(d, prev.Digest)
	return d
}

// ValidateChain checks the parent_digest links (and signatures if pub != nil). A
// break (tamper, loss, bad sig) returns error visibly. Pure unit logic.
func ValidateChain(cps []CheckpointRow, pub ed25519.PublicKey) error {
	for i := 1; i < len(cps); i++ {
		if !bytesEqual(cps[i].ParentDigest, cps[i-1].Digest) {
			return fmt.Errorf("checkpoint parent chain broken at seq %d (parent %x != prev %x)", cps[i].Seq, cps[i].ParentDigest, cps[i-1].Digest)
		}
	}
	if len(pub) == ed25519.PublicKeySize && len(cps) > 0 {
		for _, cp := range cps {
			if len(cp.Signature) > 0 && !ed25519.Verify(pub, cp.Digest, cp.Signature) {
				return fmt.Errorf("checkpoint signature invalid at seq %d", cp.Seq)
			}
		}
	}
	return nil
}

// CheckpointForSealed constructs exactly one journal_checkpoints row for a sealed
// partition: id_from/id_to, digest = ComputeDigest over the compacted rows in id
// order, parent_digest chained via ParentFor. Signature is left for the engine
// key signer. Initial location "resident". Pure unit logic.
func CheckpointForSealed(idFrom, idTo int64, compacted [][]byte, prev *CheckpointRow) CheckpointRow {
	return CheckpointRow{
		IDFrom:       idFrom,
		IDTo:         idTo,
		Digest:       ComputeDigest(compacted),
		ParentDigest: ParentFor(prev),
		Location:     "resident",
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return string(a) == string(b)
}
