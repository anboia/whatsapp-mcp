package main

import (
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"
)

func TestQuotedIDIsStored(t *testing.T) {
	store := newTestStore(t)
	reply := &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
		Text:        proto.String("on the 23rd"),
		ContextInfo: &waProto.ContextInfo{StanzaID: proto.String("Q1"), QuotedMessage: &waProto.Message{Conversation: proto.String("*1. When did the noise start?*")}},
	}}
	if got := quotedOf(reply); got.ID != "Q1" || got.Text != "*1. When did the noise start?*" {
		t.Fatalf("quotedOf = %+v", got)
	}
	if got := quotedOf(&waProto.Message{Conversation: proto.String("hi")}); got.ID != "" {
		t.Fatalf("plain message should have no quote, got %+v", got)
	}
	audio := &waProto.Message{AudioMessage: &waProto.AudioMessage{ContextInfo: &waProto.ContextInfo{StanzaID: proto.String("Q2")}}}
	if got := quotedOf(audio); got.ID != "Q2" || got.Text != "" {
		t.Fatalf("audio reply quote = %q", got)
	}
	if err := store.StoreMessage("R1", "g@g.us", "123", "", "on the 23rd", time.Now(), false, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetQuoted("R1", "g@g.us", quotedOf(reply)); err != nil {
		t.Fatal(err)
	}
	var q, qt string
	store.db.QueryRow("SELECT quoted_id, quoted_text FROM messages WHERE id = 'R1'").Scan(&q, &qt)
	if q != "Q1" || qt != "*1. When did the noise start?*" {
		t.Errorf("quoted = %q / %q", q, qt)
	}
}
