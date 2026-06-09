package protocol

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
)

// ExtraBlock carries server-signed integrity data appended after a
// channel's normal blocks. The server serves it at block index == the
// channel's normal block count. Old clients fetch only the known count
// and never request this index, so they are unaffected. New clients
// fetch it, recognise it by its magic, and verify the signature against
// the server public key pinned in the config.
//
// Decrypted plaintext layout (the caller still wraps it with the normal
// EncodeResponse encryption + base64, like any other block):
//
//	magic(2) version(1) count(1) index(1) TLV... end(0x00) padding
//
// TLV entry: type(1) len(2 BE) value(len). type 0x00 marks end-of-entries;
// everything after it is random padding. Unknown TLV types are skipped, so
// fields can be added later without breaking existing clients.
//
// The signature binds the channel id (defeats a malicious resolver swapping
// another channel's block in), a timestamp (defeats rollback to old signed
// data), and count (lets the signed payload span multiple extra blocks in a
// future version).

const (
	extraBlockMagic0  = 0x58 // 'X'
	extraBlockMagic1  = 0x42 // 'B'
	ExtraBlockVersion = 1
	// ExtraDigestSize is the truncated-SHA-256 length used for content
	// digests. 16 bytes (128-bit) is ample second-preimage resistance for
	// authenticating feed content while keeping the block small.
	ExtraDigestSize = 16

	extraHeaderSize = 5 // magic(2) + version(1) + count(1) + index(1)
)

// TLV entry types inside an ExtraBlock.
const (
	extraTLVEnd       = 0x00
	ExtraTLVTimestamp = 0x01 // int64 unix seconds, 8 bytes big-endian
	ExtraTLVDigest    = 0x02 // ExtraDigestSize bytes
	ExtraTLVSignature = 0x03 // ed25519 signature, ed25519.SignatureSize bytes
)

// extraSignContext domain-separates ExtraBlock signatures from any other
// use of the server key.
const extraSignContext = "thefeed-extrablock-v1"

// ContentDigest returns the truncated SHA-256 of canonical channel content.
// The caller is responsible for producing the canonical bytes (e.g.
// SerializeMessages / SerializeMetadata) identically on server and client.
func ContentDigest(canonical []byte) []byte {
	sum := sha256.Sum256(canonical)
	out := make([]byte, ExtraDigestSize)
	copy(out, sum[:])
	return out
}

// extraSignedData builds the deterministic byte string covered by the
// signature. Must be identical on the signing (server) and verifying
// (client) side.
func extraSignedData(channelID uint16, count uint8, ts int64, digest []byte) []byte {
	b := make([]byte, 0, len(extraSignContext)+2+1+8+len(digest))
	b = append(b, extraSignContext...)
	var tmp [8]byte
	binary.BigEndian.PutUint16(tmp[:2], channelID)
	b = append(b, tmp[:2]...)
	b = append(b, count)
	binary.BigEndian.PutUint64(tmp[:8], uint64(ts))
	b = append(b, tmp[:8]...)
	b = append(b, digest...)
	return b
}

// EncodeExtraBlock builds a single signed ExtraBlock (count=1, index=0) for
// a channel and pads it to a random size in [MinBlockPayload, MaxBlockPayload]
// so it is indistinguishable from a normal content block. ts is unix
// seconds; digest must be ExtraDigestSize bytes (see ContentDigest). The
// returned bytes are plaintext — encrypt them with EncodeResponse like any
// other block.
func EncodeExtraBlock(priv ed25519.PrivateKey, channelID uint16, digest []byte, ts int64) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("extrablock: bad private key size %d", len(priv))
	}
	if len(digest) != ExtraDigestSize {
		return nil, fmt.Errorf("extrablock: digest must be %d bytes, got %d", ExtraDigestSize, len(digest))
	}
	const count uint8 = 1
	const index uint8 = 0

	sig := ed25519.Sign(priv, extraSignedData(channelID, count, ts, digest))

	buf := make([]byte, 0, MinBlockPayload)
	buf = append(buf, extraBlockMagic0, extraBlockMagic1, ExtraBlockVersion, count, index)
	buf = appendExtraTLV(buf, ExtraTLVTimestamp, int64BE(ts))
	buf = appendExtraTLV(buf, ExtraTLVDigest, digest)
	buf = appendExtraTLV(buf, ExtraTLVSignature, sig)
	buf = append(buf, extraTLVEnd)

	target := randBlockSize()
	if target < len(buf) {
		target = len(buf)
	}
	if pad := target - len(buf); pad > 0 {
		p := make([]byte, pad)
		if _, err := rand.Read(p); err != nil {
			return nil, fmt.Errorf("extrablock padding: %w", err)
		}
		buf = append(buf, p...)
	}
	return buf, nil
}

// ExtraBlock is a parsed ExtraBlock.
type ExtraBlock struct {
	Version   uint8
	Count     uint8
	Index     uint8
	Timestamp int64
	Digest    []byte
	Signature []byte
}

// ParseExtraBlock parses a decrypted ExtraBlock plaintext. It returns an
// error (never panics) on any malformation, so a caller can treat a
// spoofed or stale block — or an old server's unrelated response — as
// "no valid extra block".
func ParseExtraBlock(data []byte) (*ExtraBlock, error) {
	if len(data) < extraHeaderSize {
		return nil, fmt.Errorf("extrablock: too short")
	}
	if data[0] != extraBlockMagic0 || data[1] != extraBlockMagic1 {
		return nil, fmt.Errorf("extrablock: bad magic")
	}
	eb := &ExtraBlock{Version: data[2], Count: data[3], Index: data[4]}
	if eb.Version != ExtraBlockVersion {
		return nil, fmt.Errorf("extrablock: unsupported version %d", eb.Version)
	}

	off := extraHeaderSize
	for off < len(data) {
		typ := data[off]
		off++
		if typ == extraTLVEnd {
			break
		}
		if off+2 > len(data) {
			return nil, fmt.Errorf("extrablock: truncated TLV length")
		}
		l := int(binary.BigEndian.Uint16(data[off:]))
		off += 2
		if off+l > len(data) {
			return nil, fmt.Errorf("extrablock: truncated TLV value")
		}
		val := data[off : off+l]
		off += l

		switch typ {
		case ExtraTLVTimestamp:
			if l != 8 {
				return nil, fmt.Errorf("extrablock: bad timestamp length %d", l)
			}
			eb.Timestamp = int64(binary.BigEndian.Uint64(val))
		case ExtraTLVDigest:
			if l != ExtraDigestSize {
				return nil, fmt.Errorf("extrablock: bad digest length %d", l)
			}
			eb.Digest = append([]byte(nil), val...)
		case ExtraTLVSignature:
			if l != ed25519.SignatureSize {
				return nil, fmt.Errorf("extrablock: bad signature length %d", l)
			}
			eb.Signature = append([]byte(nil), val...)
		default:
			// Unknown type — skip for forward compatibility.
		}
	}

	if eb.Digest == nil || eb.Signature == nil {
		return nil, fmt.Errorf("extrablock: missing digest or signature")
	}
	return eb, nil
}

// VerifyExtraBlock checks the signature against the server public key,
// binding it to channelID. channelID is the channel the client actually
// requested — if a resolver served a different channel's extra block, the
// signed channel id will not match and verification fails.
func VerifyExtraBlock(pub ed25519.PublicKey, channelID uint16, eb *ExtraBlock) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("extrablock: bad public key size %d", len(pub))
	}
	if len(eb.Signature) != ed25519.SignatureSize || len(eb.Digest) != ExtraDigestSize {
		return fmt.Errorf("extrablock: incomplete block")
	}
	signed := extraSignedData(channelID, eb.Count, eb.Timestamp, eb.Digest)
	if !ed25519.Verify(pub, signed, eb.Signature) {
		return fmt.Errorf("extrablock: signature verification failed")
	}
	return nil
}

// VerifyChannelContent confirms canonical content matches the signed
// digest. Call only after VerifyExtraBlock has succeeded.
func (eb *ExtraBlock) VerifyChannelContent(canonical []byte) error {
	want := ContentDigest(canonical)
	if subtle.ConstantTimeCompare(want, eb.Digest) != 1 {
		return fmt.Errorf("extrablock: content digest mismatch")
	}
	return nil
}

func appendExtraTLV(dst []byte, typ byte, val []byte) []byte {
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(val)))
	dst = append(dst, typ)
	dst = append(dst, l[:]...)
	return append(dst, val...)
}

func int64BE(v int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(v))
	return b
}
