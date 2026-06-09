package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// channelBlockCount reads a channel's served block count out of metadata.
func channelBlockCount(t *testing.T, feed *Feed, channelIdx int) int {
	t.Helper()
	md, err := feed.GetBlock(protocol.MetadataChannel, 0)
	if err != nil {
		t.Fatalf("metadata block: %v", err)
	}
	meta, err := protocol.ParseMetadata(md)
	if err != nil {
		t.Fatalf("parse metadata: %v", err)
	}
	return int(meta.Channels[channelIdx].Blocks)
}

func TestFeedExtraBlockSigned(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	feed := NewFeed([]string{"Sig"})
	feed.UpdateChannel(1, []protocol.Message{{ID: 1, Timestamp: 1, Text: "hello"}})
	feed.SetSigningKey(priv)

	bc := channelBlockCount(t, feed, 0)
	if bc < 1 {
		t.Fatalf("block count = %d", bc)
	}
	// Reassemble exactly what the client would concatenate.
	var content []byte
	for i := 0; i < bc; i++ {
		b, err := feed.GetBlock(1, i)
		if err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
		content = append(content, b...)
	}
	// The signed ExtraBlock is served at index == block count.
	raw, err := feed.GetBlock(1, bc)
	if err != nil {
		t.Fatalf("extra block: %v", err)
	}
	eb, err := protocol.ParseExtraBlock(raw)
	if err != nil {
		t.Fatalf("parse extra block: %v", err)
	}
	if err := protocol.VerifyExtraBlock(pub, 1, eb); err != nil {
		t.Fatalf("verify signature: %v", err)
	}
	if err := eb.VerifyChannelContent(content); err != nil {
		t.Fatalf("content digest mismatch: %v", err)
	}
	// Bound to the channel id: presenting it as another channel must fail.
	if err := protocol.VerifyExtraBlock(pub, 2, eb); err == nil {
		t.Error("expected verification to fail for the wrong channel id")
	}
}

func TestFeedNoExtraBlockWithoutKey(t *testing.T) {
	feed := NewFeed([]string{"NoSig"})
	feed.UpdateChannel(1, []protocol.Message{{ID: 1, Timestamp: 1, Text: "x"}})
	// No SetSigningKey → index == block count is just out of range.
	bc := channelBlockCount(t, feed, 0)
	if _, err := feed.GetBlock(1, bc); err == nil {
		t.Error("expected no extra block when signing is disabled")
	}
}

func TestFeedUpdateAndGetBlock(t *testing.T) {
	feed := NewFeed([]string{"TestChannel"})
	msgs := []protocol.Message{
		{ID: 1, Timestamp: 1700000000, Text: "First message"},
		{ID: 2, Timestamp: 1700000060, Text: "Second message"},
	}
	feed.UpdateChannel(1, msgs)
	data, err := feed.GetBlock(1, 0)
	if err != nil {
		t.Fatalf("GetBlock(1, 0): %v", err)
	}
	if len(data) == 0 {
		t.Error("block data should not be empty")
	}
	// Data is now compressed — decompress + parse
	decompressed, err := protocol.DecompressMessages(data)
	if err != nil {
		t.Fatalf("DecompressMessages: %v", err)
	}
	parsed, err := protocol.ParseMessages(decompressed)
	if err != nil {
		t.Fatalf("ParseMessages: %v", err)
	}
	if len(parsed) != 2 {
		t.Errorf("got %d messages, want 2", len(parsed))
	}
}

func TestFeedMetadataBlock(t *testing.T) {
	feed := NewFeed([]string{"Channel1", "Channel2"})
	msgs := []protocol.Message{{ID: 10, Timestamp: 1700000000, Text: "Hello"}}
	feed.UpdateChannel(1, msgs)
	data, err := feed.GetBlock(protocol.MetadataChannel, 0)
	if err != nil {
		t.Fatalf("GetBlock(0, 0): %v", err)
	}
	meta, err := protocol.ParseMetadata(data)
	if err != nil {
		t.Fatalf("ParseMetadata: %v", err)
	}
	if len(meta.Channels) != 2 {
		t.Fatalf("channels: got %d, want 2", len(meta.Channels))
	}
	if meta.Channels[0].Name != "Channel1" {
		t.Errorf("name: got %q, want Channel1", meta.Channels[0].Name)
	}
	if meta.Channels[0].Blocks != 1 {
		t.Errorf("blocks: got %d, want 1", meta.Channels[0].Blocks)
	}
}

func TestFeedGetBlockOutOfRange(t *testing.T) {
	feed := NewFeed([]string{"Test"})
	feed.UpdateChannel(1, []protocol.Message{{ID: 1, Timestamp: 1, Text: "x"}})
	_, err := feed.GetBlock(1, 999)
	if err == nil {
		t.Error("expected error for out-of-range block")
	}
}

func TestFeedGetBlockUnknownChannel(t *testing.T) {
	feed := NewFeed([]string{"Test"})
	_, err := feed.GetBlock(99, 0)
	if err == nil {
		t.Error("expected error for unknown channel")
	}
}

func TestFeedLargeMessages(t *testing.T) {
	feed := NewFeed([]string{"Test"})
	// With compression, repetitive data compresses to ~1 block.
	// Use varied text so compressed size still spans multiple blocks.
	largeText := make([]byte, 1500)
	for i := range largeText {
		largeText[i] = byte(i % 251) // pseudo-random pattern
	}
	msgs := []protocol.Message{{ID: 1, Timestamp: 1700000000, Text: string(largeText)}}
	feed.UpdateChannel(1, msgs)
	// Should have at least 1 block
	data0, err := feed.GetBlock(1, 0)
	if err != nil {
		t.Fatalf("GetBlock(1, 0): %v", err)
	}
	if len(data0) == 0 {
		t.Error("block data should not be empty")
	}
}

func TestApplyTextURLEntities(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		entities []tg.MessageEntityClass
		want     string
	}{
		{
			name: "no entities",
			text: "hello world",
			want: "hello world",
		},
		{
			name: "text url entity",
			text: "Check out this link for details",
			entities: []tg.MessageEntityClass{
				&tg.MessageEntityTextURL{Offset: 10, Length: 9, URL: "https://example.com"},
			},
			want: "Check out [this link](https://example.com) for details",
		},
		{
			name: "display text equals url",
			text: "Visit https://example.com today",
			entities: []tg.MessageEntityClass{
				&tg.MessageEntityTextURL{Offset: 6, Length: 19, URL: "https://example.com"},
			},
			want: "Visit https://example.com today",
		},
		{
			name: "javascript url rejected",
			text: "click here to win",
			entities: []tg.MessageEntityClass{
				&tg.MessageEntityTextURL{Offset: 0, Length: 10, URL: "javascript:alert(1)"},
			},
			want: "click here to win",
		},
		{
			name: "multiple entities",
			text: "see first and second links",
			entities: []tg.MessageEntityClass{
				&tg.MessageEntityTextURL{Offset: 4, Length: 5, URL: "https://one.com"},
				&tg.MessageEntityTextURL{Offset: 14, Length: 6, URL: "https://two.com"},
			},
			want: "see [first](https://one.com) and [second](https://two.com) links",
		},
		{
			name: "emoji in text (surrogate pair)",
			text: "📊 click here",
			entities: []tg.MessageEntityClass{
				&tg.MessageEntityTextURL{Offset: 3, Length: 10, URL: "https://poll.com"},
			},
			want: "📊 [click here](https://poll.com)",
		},
		{
			name: "non-text-url entities ignored",
			text: "bold text here",
			entities: []tg.MessageEntityClass{
				&tg.MessageEntityBold{Offset: 0, Length: 4},
			},
			want: "bold text here",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := applyTextURLEntities(tt.text, tt.entities)
			if got != tt.want {
				t.Errorf("applyTextURLEntities() = %q, want %q", got, tt.want)
			}
		})
	}
}
