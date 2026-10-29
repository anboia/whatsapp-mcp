package main

import (
	"context"
	"crypto/sha256"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDirectPathFromURL(t *testing.T) {
	cases := map[string]string{
		"https://mmg.whatsapp.net/v/t62.7117-24/1_2_n.enc?ccb=11-4&oh=01_A&oe=68F0&_nc_sid=5e03e0&mms3=true": "/v/t62.7117-24/1_2_n.enc?ccb=11-4&oh=01_A&oe=68F0&_nc_sid=5e03e0",
		"https://mmg.whatsapp.net/o1/v/t24/f2/m231/AQO-x?ccb=9-4&oh=01_B&oe=68F1&_nc_sid=e6ed6c&mms3=true":   "/o1/v/t24/f2/m231/AQO-x?ccb=9-4&oh=01_B&oe=68F1&_nc_sid=e6ed6c",
		"https://mmg.whatsapp.net/d/f/abc.enc": "/d/f/abc.enc",
		"":                                     "",
	}
	for in, want := range cases {
		if got := directPathFromURL(in); got != want {
			t.Errorf("directPathFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildMediaFilename(t *testing.T) {
	loc := time.FixedZone("BRT", -3*3600)
	time.Local = loc
	ts := time.Date(2026, 10, 1, 22, 55, 50, 0, loc).UTC()
	cases := []struct{ id, typ, mime, orig, want string }{
		{"3EB0A1B2C3D4E5F60718", "audio", "audio/ogg; codecs=opus", "", "20261001_225550_audio_3EB0A1B2C3D4E5F60718.ogg"},
		{"3A00", "image", "", "", "20261001_225550_image_3A00.jpg"},
		{"3A01", "image", "image/png", "", "20261001_225550_image_3A01.png"},
		{"3A02", "document", "application/pdf", "Invoice nº 12.pdf", "20261001_225550_document_3A02_Invoice_n_12.pdf"},
		{"3A03", "document", "application/pdf", "README", "20261001_225550_document_3A03_README.pdf"},
		{"3A04", "video", "video/mp4", "", "20261001_225550_video_3A04.mp4"},
		{"../x", "audio", "", "", "20261001_225550_audio_x.ogg"},
	}
	for _, c := range cases {
		if got := buildMediaFilename(c.id, ts, c.typ, c.mime, c.orig); got != c.want {
			t.Errorf("buildMediaFilename(%q, %q, %q, %q) = %q, want %q", c.id, c.typ, c.mime, c.orig, got, c.want)
		}
	}
}

func TestExtForUnknownMime(t *testing.T) {
	if got := extForMime("application/octet-stream", "audio"); got != ".bin" {
		t.Errorf("unknown MIME should not be named .ogg, got %q", got)
	}
	if got := extForMime("", "audio"); got != ".ogg" {
		t.Errorf("missing MIME should fall back to the WhatsApp default, got %q", got)
	}
}

// newTestStore opens a fresh store in a temporary working directory
func newTestStore(t *testing.T) *MessageStore {
	t.Helper()
	wd, _ := os.Getwd()
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })
	store, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	store.StoreChat("g@g.us", "g", time.Now())
	return store
}

func TestStoreMessageKeepsLearnedFields(t *testing.T) {
	store := newTestStore(t)
	ts := time.Date(2026, 10, 1, 22, 55, 50, 0, time.UTC)
	media := &MediaInfo{Type: "audio", URL: "https://mmg.whatsapp.net/v/a.enc?x=1", MediaKey: []byte{1}, FileSHA256: []byte{2}, FileEncSHA256: []byte{3}, FileLength: 9}
	if err := store.StoreMessage("M1", "g@g.us", "123", "", "", ts, false, media); err != nil {
		t.Fatal(err)
	}
	// What a phone re-upload and sniffing write
	store.db.Exec("UPDATE messages SET direct_path = '/fresh?y=2', mime_type = 'audio/ogg' WHERE id = 'M1'")
	// The same message arrives again from a history sync, now with a caption and the sender JID
	again := &MediaInfo{Type: "audio", URL: "https://mmg.whatsapp.net/v/a.enc?x=1", MediaKey: []byte{1}, FileSHA256: []byte{2}, FileEncSHA256: []byte{3}, DirectPath: "/old?x=1", IsForwarded: true}
	if err := store.StoreMessage("M1", "g@g.us", "123@lid", "123@lid", "caption", ts, false, again); err != nil {
		t.Fatal(err)
	}
	r, err := store.getMediaRow("M1", "g@g.us")
	if err != nil {
		t.Fatal(err)
	}
	if r.DirectPath != "/fresh?y=2" || r.MimeType != "audio/ogg" || r.SenderJID != "123@lid" || r.FileLength != 9 {
		t.Errorf("learned fields lost: %+v", r)
	}
	if r.IsForwarded == nil || !*r.IsForwarded {
		t.Errorf("is_forwarded should be filled by the new copy")
	}
	var content string
	store.db.QueryRow("SELECT content FROM messages WHERE id = 'M1'").Scan(&content)
	if content != "caption" {
		t.Errorf("caption not stored: %q", content)
	}
}

func TestFindVerifiedMediaIgnoresNameAndRejectsOtherContent(t *testing.T) {
	store := newTestStore(t)
	data := []byte("OggS fake audio")
	sum := sha256.Sum256(data)
	r := &mediaRow{ID: "3AE6", MediaType: "audio", FileSHA256: sum[:]}
	dir := "store/g@g.us"
	os.MkdirAll(dir, 0755)
	_ = store
	// Saved while the computer was in another timezone: the time in the name differs
	os.WriteFile(dir+"/20261001_215550_audio_3AE6.ogg", data, 0644)
	os.WriteFile(dir+"/20261001_225550_audio_3AE6.ogg.part", data, 0644)
	if path, _, _, ok := findVerifiedMedia(dir, r); !ok || !strings.HasSuffix(path, "215550_audio_3AE6.ogg") {
		t.Errorf("verified file under another name not found: %q %v", path, ok)
	}
	os.WriteFile(dir+"/20261001_215550_audio_3AE6.ogg", []byte("edited"), 0644)
	if _, _, _, ok := findVerifiedMedia(dir, r); ok {
		t.Errorf("a file with different content was accepted")
	}
}

func TestLockMessageHonoursContext(t *testing.T) {
	unlock, err := lockMessage(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := lockMessage(ctx, "k"); err == nil {
		t.Errorf("second lock should give up when its context ends")
	}
	unlock()
	if u, err := lockMessage(context.Background(), "k"); err != nil {
		t.Errorf("lock not released: %v", err)
	} else {
		u()
	}
}

func TestFindVerifiedMediaIsAnchored(t *testing.T) {
	newTestStore(t)
	data := []byte("same bytes")
	sum := sha256.Sum256(data)
	dir := "store/g@g.us"
	os.MkdirAll(dir, 0755)
	// An exported image sent back to the chat as a document keeps the image's name inside its own
	os.WriteFile(dir+"/20261005_100000_document_NEWDOC_20261001_221238_image_3A00.jpg", data, 0644)
	img := &mediaRow{ID: "3A00", MediaType: "image", FileSHA256: sum[:]}
	if path, _, _, ok := findVerifiedMedia(dir, img); ok {
		t.Errorf("another message's document was accepted as the image: %s", path)
	}
	doc := &mediaRow{ID: "NEWDOC", MediaType: "document", FileSHA256: sum[:]}
	if _, _, _, ok := findVerifiedMedia(dir, doc); !ok {
		t.Errorf("the document's own file was not found")
	}
}

func TestStoreChatIfNewer(t *testing.T) {
	store := newTestStore(t)
	late := time.Date(2026, 10, 2, 9, 33, 17, 0, time.FixedZone("BRT", -3*3600))
	store.StoreChat("c@g.us", "c", late)
	store.StoreChatIfNewer("c@g.us", "c", late.Add(-3*time.Minute))
	var got time.Time
	store.db.QueryRow("SELECT last_message_time FROM chats WHERE jid = 'c@g.us'").Scan(&got)
	if !got.Equal(late) {
		t.Errorf("older history moved the chat back to %v", got)
	}
	store.StoreChatIfNewer("c@g.us", "c", late.Add(time.Minute))
	store.db.QueryRow("SELECT last_message_time FROM chats WHERE jid = 'c@g.us'").Scan(&got)
	if !got.Equal(late.Add(time.Minute)) {
		t.Errorf("newer time not stored: %v", got)
	}
}
