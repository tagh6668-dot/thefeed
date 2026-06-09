package client

import (
	"context"
	"crypto/ed25519"
	cryptoRand "crypto/rand"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// buildSignedChannel serializes msgs the way the server does and returns the
// content blocks plus a signed ExtraBlock over their concatenation.
func buildSignedChannel(t *testing.T, priv ed25519.PrivateKey, ch uint16, msgs []protocol.Message) ([][]byte, []byte) {
	t.Helper()
	raw := protocol.CompressMessages(protocol.SerializeMessages(msgs))
	blocks := protocol.SplitIntoBlocks(raw)
	var concat []byte
	for _, b := range blocks {
		concat = append(concat, b...)
	}
	eb, err := protocol.EncodeExtraBlock(priv, ch, protocol.ContentDigest(concat), time.Now().Unix())
	if err != nil {
		t.Fatalf("encode extra block: %v", err)
	}
	return blocks, eb
}

// routeExchange decodes each query and serves the matching content block, the
// ExtraBlock at index == len(blocks), or an error beyond that.
func routeExchange(f *Fetcher, ch uint16, blocks [][]byte, extra []byte) func(context.Context, *dns.Msg, string) (*dns.Msg, time.Duration, error) {
	return func(ctx context.Context, m *dns.Msg, addr string) (*dns.Msg, time.Duration, error) {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		qname := m.Question[0].Name
		c, blk, err := protocol.DecodeQuery(f.queryKey, qname, f.domain)
		if err != nil {
			return nil, 0, err
		}
		if c != ch {
			return nil, 0, fmt.Errorf("unexpected channel %d", c)
		}
		var payload []byte
		switch {
		case int(blk) < len(blocks):
			payload = blocks[blk]
		case int(blk) == len(blocks):
			payload = extra
		default:
			return nil, 0, fmt.Errorf("no block %d", blk)
		}
		encoded, err := protocol.EncodeResponse(f.responseKey, payload, 0)
		if err != nil {
			return nil, 0, err
		}
		resp := new(dns.Msg)
		resp.SetReply(m)
		resp.Rcode = dns.RcodeSuccess
		resp.Answer = []dns.RR{&dns.TXT{
			Hdr: dns.RR_Header{Name: qname, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 0},
			Txt: []string{encoded},
		}}
		return resp, time.Millisecond, nil
	}
}

func TestFetchChannelAcceptsValidSignature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(cryptoRand.Reader)
	const ch uint16 = 1
	blocks, extra := buildSignedChannel(t, priv, ch, []protocol.Message{{ID: 1, Timestamp: 1, Text: "hello"}})

	f := newTestFetcher(t, []string{"1.1.1.1:53"})
	f.scatter = 1
	if err := f.SetServerPublicKey(base64.RawURLEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	f.exchangeFn = routeExchange(f, ch, blocks, extra)

	got, err := f.FetchChannel(context.Background(), int(ch), len(blocks))
	if err != nil {
		t.Fatalf("FetchChannel: %v", err)
	}
	if len(got) != 1 || got[0].Text != "hello" {
		t.Errorf("messages = %+v, want one 'hello'", got)
	}
}

func TestFetchChannelRejectsTamperedContent(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(cryptoRand.Reader)
	const ch uint16 = 1
	// Sign the genuine content, but serve different ("evil") content with the
	// still-valid signature — a malicious resolver swapping the payload.
	_, extra := buildSignedChannel(t, priv, ch, []protocol.Message{{ID: 1, Timestamp: 1, Text: "hello"}})
	tampered := protocol.SplitIntoBlocks(protocol.CompressMessages(protocol.SerializeMessages(
		[]protocol.Message{{ID: 1, Timestamp: 1, Text: "EVIL"}})))

	f := newTestFetcher(t, []string{"1.1.1.1:53"})
	f.scatter = 1
	if err := f.SetServerPublicKey(base64.RawURLEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	f.exchangeFn = routeExchange(f, ch, tampered, extra)

	if _, err := f.FetchChannel(context.Background(), int(ch), len(tampered)); err == nil {
		t.Fatal("expected FetchChannel to reject tampered content")
	}
}
