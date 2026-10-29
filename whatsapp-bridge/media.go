package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// How long to wait for the phone to re-upload expired media
const mediaRetryTimeout = 60 * time.Second

// Upper bound for one download requested over the API, including a phone re-upload
const downloadTimeout = 30 * time.Minute

// Chats listed in this file (one JID per line, "*" for all) get their media downloaded on arrival
const autoDownloadFile = "store/autodownload.txt"

// MediaInfo holds everything needed to download and describe an attachment
type MediaInfo struct {
	Type          string // image, video, audio or document
	Filename      string // original name for documents, generated name otherwise
	URL           string
	DirectPath    string
	MimeType      string
	MediaKey      []byte
	FileSHA256    []byte
	FileEncSHA256 []byte
	FileLength    uint64
	Duration      uint32 // seconds, audio and video only
	IsForwarded   bool
}

// Extract media info from a message
func extractMediaInfo(msg *waProto.Message) *MediaInfo {
	if msg == nil {
		return nil
	}

	if img := msg.GetImageMessage(); img != nil {
		return &MediaInfo{
			Type: "image", URL: img.GetURL(), DirectPath: img.GetDirectPath(), MimeType: img.GetMimetype(),
			MediaKey: img.GetMediaKey(), FileSHA256: img.GetFileSHA256(), FileEncSHA256: img.GetFileEncSHA256(),
			FileLength: img.GetFileLength(), IsForwarded: img.GetContextInfo().GetIsForwarded(),
		}
	}

	vid := msg.GetVideoMessage()
	if vid == nil {
		// Round video notes
		vid = msg.GetPtvMessage()
	}
	if vid != nil {
		return &MediaInfo{
			Type: "video", URL: vid.GetURL(), DirectPath: vid.GetDirectPath(), MimeType: vid.GetMimetype(),
			MediaKey: vid.GetMediaKey(), FileSHA256: vid.GetFileSHA256(), FileEncSHA256: vid.GetFileEncSHA256(),
			FileLength: vid.GetFileLength(), Duration: vid.GetSeconds(), IsForwarded: vid.GetContextInfo().GetIsForwarded(),
		}
	}

	if aud := msg.GetAudioMessage(); aud != nil {
		return &MediaInfo{
			Type: "audio", URL: aud.GetURL(), DirectPath: aud.GetDirectPath(), MimeType: aud.GetMimetype(),
			MediaKey: aud.GetMediaKey(), FileSHA256: aud.GetFileSHA256(), FileEncSHA256: aud.GetFileEncSHA256(),
			FileLength: aud.GetFileLength(), Duration: aud.GetSeconds(), IsForwarded: aud.GetContextInfo().GetIsForwarded(),
		}
	}

	if doc := msg.GetDocumentMessage(); doc != nil {
		return &MediaInfo{
			Type: "document", Filename: doc.GetFileName(), URL: doc.GetURL(), DirectPath: doc.GetDirectPath(), MimeType: doc.GetMimetype(),
			MediaKey: doc.GetMediaKey(), FileSHA256: doc.GetFileSHA256(), FileEncSHA256: doc.GetFileEncSHA256(),
			FileLength: doc.GetFileLength(), IsForwarded: doc.GetContextInfo().GetIsForwarded(),
		}
	}

	return nil
}

var mimeExtensions = map[string]string{
	"image/jpeg":       ".jpg",
	"image/png":        ".png",
	"image/webp":       ".webp",
	"image/gif":        ".gif",
	"image/heic":       ".heic",
	"image/heif":       ".heif",
	"audio/ogg":        ".ogg",
	"audio/opus":       ".opus",
	"audio/mpeg":       ".mp3",
	"audio/mp4":        ".m4a",
	"audio/aac":        ".aac",
	"audio/amr":        ".amr",
	"audio/wav":        ".wav",
	"audio/x-wav":      ".wav",
	"audio/wave":       ".wav",
	"audio/webm":       ".webm",
	"video/mp4":        ".mp4",
	"video/3gpp":       ".3gp",
	"video/quicktime":  ".mov",
	"video/webm":       ".webm",
	"audio/x-m4a":      ".m4a",
	"audio/3gpp":       ".3gp",
	"video/x-matroska": ".mkv",
	"image/bmp":        ".bmp",
	"image/tiff":       ".tiff",
	"text/plain":       ".txt",
	"application/zip":  ".zip",
	"application/pdf":  ".pdf",
	"application/ogg":  ".ogg",
}

// File extension for a MIME type, falling back to the usual WhatsApp format for the media type
func extForMime(mimeType, mediaType string) string {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0]))
	if ext, ok := mimeExtensions[base]; ok {
		return ext
	}
	if base != "" {
		// A type we don't know (or application/octet-stream): don't pretend it is a JPEG or an OGG
		return ".bin"
	}
	switch mediaType {
	case "image":
		return ".jpg"
	case "audio":
		return ".ogg"
	case "video":
		return ".mp4"
	}
	return ".bin"
}

// Guess the MIME type from the file content, for messages stored without one
func sniffMime(data []byte, mediaType string) string {
	sniffed := http.DetectContentType(data)
	switch {
	case mediaType == "audio" && (sniffed == "application/ogg" || bytes.HasPrefix(data, []byte("OggS"))):
		return "audio/ogg"
	case mediaType == "audio" && sniffed == "video/mp4":
		return "audio/mp4"
	}
	return sniffed
}

var unsafeNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(name string) string {
	name = strings.Trim(unsafeNameChars.ReplaceAllString(name, "_"), "._")
	if len(name) > 120 {
		name = name[len(name)-120:]
	}
	return name
}

// Build a file name that is unique per message and sorts by date:
// 20261001_225550_audio_3EB0A1B2C3D4E5F60718.ogg
func buildMediaFilename(id string, timestamp time.Time, mediaType, mimeType, originalName string) string {
	base := fmt.Sprintf("%s_%s_%s", timestamp.In(time.Local).Format("20060102_150405"), mediaType, safeName(id))
	if mediaType == "document" {
		if orig := safeName(originalName); orig != "" {
			if filepath.Ext(orig) == "" {
				orig += extForMime(mimeType, mediaType)
			}
			return base + "_" + orig
		}
	}
	return base + extForMime(mimeType, mediaType)
}

// Rebuild the CDN direct path from a stored media URL. The query string carries the
// signature WhatsApp checks (oh/oe), so it has to stay; only the mms3 flag is dropped.
func directPathFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Path == "" {
		return ""
	}
	path := u.EscapedPath()
	var keep []string
	for _, part := range strings.Split(u.RawQuery, "&") {
		if part != "" && part != "mms3=true" {
			keep = append(keep, part)
		}
	}
	if len(keep) > 0 {
		path += "?" + strings.Join(keep, "&")
	}
	return path
}

func whatsmeowMediaType(mediaType string) (whatsmeow.MediaType, error) {
	switch mediaType {
	case "image":
		return whatsmeow.MediaImage, nil
	case "video":
		return whatsmeow.MediaVideo, nil
	case "audio":
		return whatsmeow.MediaAudio, nil
	case "document":
		return whatsmeow.MediaDocument, nil
	}
	return "", fmt.Errorf("unsupported media type: %s", mediaType)
}

// mediaRow is a media message as stored in messages.db
type mediaRow struct {
	ID, ChatJID, Sender, SenderJID      string
	Timestamp                           time.Time
	IsFromMe                            bool
	MediaType, Filename, MimeType       string
	URL, DirectPath                     string
	MediaKey, FileSHA256, FileEncSHA256 []byte
	FileLength                          uint64
	Duration                            uint32
	IsForwarded                         *bool // nil when stored before is_forwarded existed
}

func (store *MessageStore) getMediaRow(id, chatJID string) (*mediaRow, error) {
	r := mediaRow{ID: id, ChatJID: chatJID}
	var sender, mediaType, filename, mediaURL, directPath, mimeType, senderJID sql.NullString
	var fileLength, duration sql.NullInt64
	var isForwarded sql.NullBool
	err := store.db.QueryRow(
		`SELECT sender, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256,
		        file_length, direct_path, mime_type, sender_jid, duration, is_forwarded
		 FROM messages WHERE id = ? AND chat_jid = ?`,
		id, chatJID,
	).Scan(&sender, &r.Timestamp, &r.IsFromMe, &mediaType, &filename, &mediaURL, &r.MediaKey, &r.FileSHA256, &r.FileEncSHA256,
		&fileLength, &directPath, &mimeType, &senderJID, &duration, &isForwarded)
	if err != nil {
		return nil, err
	}
	r.Sender, r.MediaType, r.Filename, r.URL = sender.String, mediaType.String, filename.String, mediaURL.String
	r.DirectPath, r.MimeType, r.SenderJID = directPath.String, mimeType.String, senderJID.String
	r.FileLength, r.Duration = uint64(fileLength.Int64), uint32(duration.Int64)
	if isForwarded.Valid {
		r.IsForwarded = &isForwarded.Bool
	}
	return &r, nil
}

// DownloadResult describes a media file saved to disk
type DownloadResult struct {
	MessageID    string    `json:"message_id"`
	ChatJID      string    `json:"chat_jid"`
	Timestamp    time.Time `json:"timestamp"`
	Sender       string    `json:"sender"`
	SenderJID    string    `json:"sender_jid,omitempty"`
	IsFromMe     bool      `json:"is_from_me"`
	MediaType    string    `json:"media_type"`
	MimeType     string    `json:"mime_type,omitempty"`
	Duration     uint32    `json:"duration,omitempty"`
	IsForwarded  *bool     `json:"is_forwarded,omitempty"`
	OriginalName string    `json:"original_name,omitempty"`
	Filename     string    `json:"filename"`
	Path         string    `json:"path"`
	Size         int64     `json:"size"`
	SHA256       string    `json:"sha256"`
	Cached       bool      `json:"cached"`
	MediaRetry   bool      `json:"media_retry"`
}

func (r *mediaRow) result() *DownloadResult {
	res := &DownloadResult{
		MessageID: r.ID, ChatJID: r.ChatJID, Timestamp: r.Timestamp, Sender: r.Sender, SenderJID: r.SenderJID,
		IsFromMe: r.IsFromMe, MediaType: r.MediaType, MimeType: r.MimeType, Duration: r.Duration, IsForwarded: r.IsForwarded,
	}
	if r.MediaType == "document" {
		res.OriginalName = r.Filename
	}
	return res
}

// One lock per message so a manual download and an auto-download never write the same file at once.
// A buffered channel instead of a mutex lets a waiter give up when its context ends.
var downloadLocks sync.Map

func lockMessage(ctx context.Context, key string) (func(), error) {
	l, _ := downloadLocks.LoadOrStore(key, make(chan struct{}, 1))
	lock := l.(chan struct{})
	select {
	case lock <- struct{}{}:
		return func() { <-lock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Pending media retry requests, keyed by message ID
type mediaRetryHub struct {
	mu      sync.Mutex
	waiters map[types.MessageID]chan *events.MediaRetry
}

var retryHub = &mediaRetryHub{waiters: make(map[types.MessageID]chan *events.MediaRetry)}

func (h *mediaRetryHub) wait(id types.MessageID) (chan *events.MediaRetry, func()) {
	ch := make(chan *events.MediaRetry, 1)
	h.mu.Lock()
	h.waiters[id] = ch
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if h.waiters[id] == ch {
			delete(h.waiters, id)
		}
		h.mu.Unlock()
	}
}

func (h *mediaRetryHub) dispatch(evt *events.MediaRetry) bool {
	h.mu.Lock()
	ch := h.waiters[evt.MessageID]
	h.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- evt:
	default:
	}
	return true
}

// Save the new path from a re-upload answer that arrived after its request gave up,
// so the next download can use it without asking the phone again
func saveLateMediaRetry(store *MessageStore, evt *events.MediaRetry) {
	var mediaKey []byte
	chatJID := evt.ChatID.String()
	err := store.db.QueryRow("SELECT media_key FROM messages WHERE id = ? AND chat_jid = ?", evt.MessageID, chatJID).Scan(&mediaKey)
	if err != nil || len(mediaKey) == 0 {
		return
	}
	notif, err := whatsmeow.DecryptMediaRetryNotification(evt, mediaKey)
	if err != nil || notif.GetResult() != waMmsRetry.MediaRetryNotification_SUCCESS || notif.GetDirectPath() == "" {
		return
	}
	if _, err := store.db.Exec("UPDATE messages SET direct_path = ? WHERE id = ? AND chat_jid = ?", notif.GetDirectPath(), evt.MessageID, chatJID); err == nil {
		fmt.Printf("Saved late media re-upload for message %s in %s\n", evt.MessageID, chatJID)
	}
}

// Find the full JID of the message sender, which media retry receipts need for group messages.
// Rows stored before sender_jid existed only have the user part, so look it up in the group.
func resolveSenderJID(ctx context.Context, client *whatsmeow.Client, r *mediaRow, chat types.JID) types.JID {
	if r.SenderJID != "" {
		if jid, err := types.ParseJID(r.SenderJID); err == nil {
			return jid.ToNonAD()
		}
	}
	user := r.Sender
	if strings.Contains(user, "@") {
		if jid, err := types.ParseJID(user); err == nil {
			return jid.ToNonAD()
		}
	}
	if chat.Server == types.GroupServer && user != "" {
		if info, err := client.GetGroupInfo(ctx, chat); err == nil {
			for _, p := range info.Participants {
				if p.JID.User == user || p.LID.User == user || p.PhoneNumber.User == user {
					return p.JID
				}
			}
		}
	}
	if r.IsFromMe && client.Store.ID != nil {
		return client.Store.ID.ToNonAD()
	}
	if user == "" {
		return chat
	}
	// Rows stored before sender_jid existed may hold a bare LID; the LID map tells them apart from phone numbers
	lid := types.NewJID(user, types.HiddenUserServer)
	if pn, err := client.Store.LIDs.GetPNForLID(ctx, lid); err == nil && !pn.IsEmpty() {
		return lid
	}
	return types.NewJID(user, types.DefaultUserServer)
}

// errPhoneNoAnswer means the phone did not answer a re-upload request in time
var errPhoneNoAnswer = errors.New("no answer from the phone (it must be online to re-upload media)")

func isExpiredMediaError(err error) bool {
	return errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410)
}

// Ask the phone to upload the media again and return the new direct path
func requestMediaRetry(ctx context.Context, client *whatsmeow.Client, store *MessageStore, r *mediaRow) (string, error) {
	chat, err := types.ParseJID(r.ChatJID)
	if err != nil {
		return "", fmt.Errorf("invalid chat JID: %v", err)
	}
	info := &types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     chat,
			Sender:   resolveSenderJID(ctx, client, r, chat),
			IsFromMe: r.IsFromMe,
			IsGroup:  chat.Server == types.GroupServer,
		},
		ID: types.MessageID(r.ID),
	}

	ch, done := retryHub.wait(info.ID)
	defer done()
	if err := client.SendMediaRetryReceipt(ctx, info, r.MediaKey); err != nil {
		return "", fmt.Errorf("failed to send media retry request: %v", err)
	}

	select {
	case evt := <-ch:
		notif, err := whatsmeow.DecryptMediaRetryNotification(evt, r.MediaKey)
		if err != nil {
			return "", err
		}
		if notif.GetResult() != waMmsRetry.MediaRetryNotification_SUCCESS || notif.GetDirectPath() == "" {
			return "", fmt.Errorf("phone could not re-upload the media (result %s)", notif.GetResult())
		}
		if _, err := store.db.Exec("UPDATE messages SET direct_path = ? WHERE id = ? AND chat_jid = ?",
			notif.GetDirectPath(), r.ID, r.ChatJID); err != nil {
			fmt.Printf("Failed to save new direct path for %s: %v\n", r.ID, err)
		}
		return notif.GetDirectPath(), nil
	case <-time.After(mediaRetryTimeout):
		return "", fmt.Errorf("%w within %s", errPhoneNoAnswer, mediaRetryTimeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func fetchMedia(ctx context.Context, client *whatsmeow.Client, r *mediaRow, directPath string) ([]byte, error) {
	waType, err := whatsmeowMediaType(r.MediaType)
	if err != nil {
		return nil, err
	}
	// whatsmeow checks both the encrypted and the decrypted SHA-256 against the message
	return client.DownloadMediaWithPath(ctx, directPath, r.FileEncSHA256, r.FileSHA256, r.MediaKey, waType, "", false)
}

func sha256File(path string) (string, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), int64(len(data)), nil
}

// Write through a temporary file so a crash never leaves a truncated file under the final name.
// The modification time is set to the message time.
func writeMediaFile(path string, data []byte, msgTime time.Time) error {
	tmp := path + ".part"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if !msgTime.IsZero() {
		os.Chtimes(tmp, msgTime, msgTime)
	}
	return os.Rename(tmp, path)
}

// Look for an earlier download of this message whose SHA-256 matches the message, whatever
// the time in its name (names carry the local time, which changes with the computer's timezone).
// Names must have exactly our shape, YYYYMMDD_HHMMSS_<type>_<id>..., so a document whose
// original name contains another message's file name never matches.
func findVerifiedMedia(chatDir string, r *mediaRow) (path, sum string, size int64, ok bool) {
	expected := hex.EncodeToString(r.FileSHA256)
	tail := `\.[A-Za-z0-9]+`
	if r.MediaType == "document" {
		tail = `(\.[A-Za-z0-9]+|_.+)`
	}
	shape := regexp.MustCompile(`^\d{8}_\d{6}_` + regexp.QuoteMeta(r.MediaType) + `_` + regexp.QuoteMeta(safeName(r.ID)) + tail + `$`)
	entries, err := os.ReadDir(chatDir)
	if err != nil {
		return "", "", 0, false
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !shape.MatchString(name) || strings.HasSuffix(name, ".part") || strings.Contains(name, ".mismatch-") {
			continue
		}
		m := filepath.Join(chatDir, name)
		if s, n, err := sha256File(m); err == nil && s == expected {
			return m, s, n, true
		}
	}
	return "", "", 0, false
}

// Download the media of a message into store/<chat>/ and verify it.
// allowRetry asks the phone to re-upload media that expired on the WhatsApp servers.
func downloadMedia(ctx context.Context, client *whatsmeow.Client, store *MessageStore, messageID, chatJID string, allowRetry bool) (*DownloadResult, error) {
	unlock, err := lockMessage(ctx, chatJID+"/"+messageID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	r, err := store.getMediaRow(messageID, chatJID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("message %s not found in chat %s", messageID, chatJID)
	} else if err != nil {
		return nil, fmt.Errorf("failed to read message: %v", err)
	}
	if r.MediaType == "" {
		return nil, fmt.Errorf("message %s has no media", messageID)
	}
	// Without the hash from the message there is no way to prove a file is this media
	if len(r.FileSHA256) == 0 {
		return nil, fmt.Errorf("the message has no file hash, so a download could not be verified")
	}
	res := r.result()

	chatDir := filepath.Join("store", strings.ReplaceAll(chatJID, ":", "_"))
	if err := os.MkdirAll(chatDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create chat directory: %v", err)
	}

	// Reuse an earlier download only if its hash matches the one in the message
	if path, sum, size, ok := findVerifiedMedia(chatDir, r); ok {
		res.Filename, res.Size, res.SHA256, res.Cached = filepath.Base(path), size, sum, true
		res.Path, _ = filepath.Abs(path)
		return res, nil
	}

	if len(r.MediaKey) == 0 || len(r.FileEncSHA256) == 0 {
		return nil, fmt.Errorf("incomplete media information for download")
	}

	// A usable direct path starts with "/"; old rows only have the URL to rebuild it from
	directPath := r.DirectPath
	if directPath == "" {
		directPath = directPathFromURL(r.URL)
	}

	var data []byte
	if strings.HasPrefix(directPath, "/") {
		fmt.Printf("Downloading %s media for message %s in chat %s...\n", r.MediaType, messageID, chatJID)
		data, err = fetchMedia(ctx, client, r, directPath)
		// A later copy of the message may have brought a fresher URL than the stored path
		if urlPath := directPathFromURL(r.URL); err != nil && isExpiredMediaError(err) && strings.HasPrefix(urlPath, "/") && urlPath != directPath {
			data, err = fetchMedia(ctx, client, r, urlPath)
		}
	} else {
		err = errors.New("no download path stored for this message")
	}
	if err != nil && allowRetry && (isExpiredMediaError(err) || !strings.HasPrefix(directPath, "/")) {
		fmt.Printf("Media for %s is not available on the server (%v), asking the phone to upload it again...\n", messageID, err)
		newPath, retryErr := requestMediaRetry(ctx, client, store, r)
		if retryErr != nil {
			return nil, fmt.Errorf("download failed (%v) and media retry failed: %w", err, retryErr)
		}
		res.MediaRetry = true
		data, err = fetchMedia(ctx, client, r, newPath)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to download media: %w", err)
	}

	// Messages stored before mime_type existed: name the file after its actual content
	if r.MimeType == "" {
		r.MimeType = sniffMime(data, r.MediaType)
		res.MimeType = r.MimeType
		store.db.Exec("UPDATE messages SET mime_type = ? WHERE id = ? AND chat_jid = ? AND (mime_type IS NULL OR mime_type = '')",
			r.MimeType, r.ID, r.ChatJID)
	}
	filename := buildMediaFilename(r.ID, r.Timestamp, r.MediaType, r.MimeType, r.Filename)
	localPath := filepath.Join(chatDir, filename)

	// Something else under our name (an edited copy?): keep it, but out of the way
	if _, err := os.Stat(localPath); err == nil {
		aside := fmt.Sprintf("%s.mismatch-%d", localPath, time.Now().Unix())
		fmt.Printf("Existing file %s does not match message %s, moving it to %s\n", localPath, messageID, aside)
		if err := os.Rename(localPath, aside); err != nil {
			return nil, fmt.Errorf("failed to move mismatched file aside: %v", err)
		}
	}

	if err := writeMediaFile(localPath, data, r.Timestamp); err != nil {
		return nil, fmt.Errorf("failed to save media file: %v", err)
	}

	sum := sha256.Sum256(data)
	res.Filename, res.Size, res.SHA256 = filename, int64(len(data)), hex.EncodeToString(sum[:])
	res.Path, _ = filepath.Abs(localPath)
	fmt.Printf("Saved %s media to %s (%d bytes)\n", r.MediaType, res.Path, len(data))
	return res, nil
}

// Ask the phone to send the chat history again (on-demand history sync). The answer arrives as a
// HistorySync event and fills captions, sender JIDs and download paths for messages stored by older
// versions of the bridge. The phone sends messages older than the newest one stored for the chat.
func requestChatHistory(ctx context.Context, client *whatsmeow.Client, store *MessageStore, chatJID string, count int) (string, error) {
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return "", fmt.Errorf("invalid chat JID: %v", err)
	}
	// Newest message by actual time (stored timestamps may carry different UTC offsets)
	rows, err := store.db.Query("SELECT id, timestamp, is_from_me, sender, sender_jid FROM messages WHERE chat_jid = ?", chatJID)
	if err != nil {
		return "", err
	}
	var anchor mediaRow
	for rows.Next() {
		var row mediaRow
		var sender, senderJID sql.NullString
		if err := rows.Scan(&row.ID, &row.Timestamp, &row.IsFromMe, &sender, &senderJID); err != nil {
			rows.Close()
			return "", err
		}
		row.Sender, row.SenderJID, row.ChatJID = sender.String, senderJID.String, chatJID
		if anchor.ID == "" || row.Timestamp.After(anchor.Timestamp) {
			anchor = row
		}
	}
	rows.Close()
	if anchor.ID == "" {
		return "", fmt.Errorf("no stored message in %s to anchor the request", chatJID)
	}

	info := &types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, IsFromMe: anchor.IsFromMe, IsGroup: chat.Server == types.GroupServer},
		ID:            types.MessageID(anchor.ID),
		Timestamp:     anchor.Timestamp,
	}
	if _, err := client.SendPeerMessage(ctx, client.BuildHistorySyncRequest(info, count)); err != nil {
		return "", fmt.Errorf("failed to send history request to the phone: %v", err)
	}
	// The history request only covers messages before the anchor; ask for the anchor on its own
	sender := resolveSenderJID(ctx, client, &anchor, chat)
	if _, err := client.SendPeerMessage(ctx, client.BuildUnavailableMessageRequest(chat, sender, anchor.ID)); err != nil {
		fmt.Printf("Failed to ask the phone to resend message %s: %v\n", anchor.ID, err)
	}
	return anchor.ID, nil
}

// Media message IDs of a chat, oldest first, optionally filtered by time and type
func (store *MessageStore) listMediaMessages(chatJID string, after, before time.Time, mediaTypes []string) ([]string, error) {
	rows, err := store.db.Query(
		"SELECT id, timestamp, media_type FROM messages WHERE chat_jid = ? AND media_type IS NOT NULL AND media_type <> '' ORDER BY timestamp",
		chatJID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	wanted := make(map[string]bool)
	for _, t := range mediaTypes {
		wanted[strings.ToLower(strings.TrimSpace(t))] = true
	}

	var ids []string
	for rows.Next() {
		var id, mediaType string
		var ts time.Time
		if err := rows.Scan(&id, &ts, &mediaType); err != nil {
			return nil, err
		}
		if (!after.IsZero() && ts.Before(after)) || (!before.IsZero() && !ts.Before(before)) {
			continue
		}
		if len(wanted) > 0 && !wanted[mediaType] {
			continue
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func autoDownloadEnabled(chatJID string) bool {
	data, err := os.ReadFile(autoDownloadFile)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if line == "*" || line == chatJID {
			return true
		}
	}
	return false
}

var autoDownloadSlots = make(chan struct{}, 2)

// Download new media right away for chats listed in store/autodownload.txt
func maybeAutoDownload(client *whatsmeow.Client, store *MessageStore, messageID, chatJID string) {
	if !autoDownloadEnabled(chatJID) {
		return
	}
	go func() {
		autoDownloadSlots <- struct{}{}
		defer func() { <-autoDownloadSlots }()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if res, err := downloadMedia(ctx, client, store, messageID, chatJID, true); err != nil {
			fmt.Printf("Auto-download of %s in %s failed: %v\n", messageID, chatJID, err)
		} else if !res.Cached {
			fmt.Printf("Auto-downloaded %s (%s)\n", res.Path, res.SHA256)
		}
	}()
}

// DownloadMediaRequest represents the request body for the download media API
type DownloadMediaRequest struct {
	MessageID  string `json:"message_id"`
	ChatJID    string `json:"chat_jid"`
	MediaRetry *bool  `json:"media_retry,omitempty"` // default true
}

// DownloadMediaResponse represents the response for the download media API
type DownloadMediaResponse struct {
	Success  bool            `json:"success"`
	Message  string          `json:"message"`
	Filename string          `json:"filename,omitempty"`
	Path     string          `json:"path,omitempty"`
	Media    *DownloadResult `json:"media,omitempty"`
}

// DownloadChatRequest represents the request body for the download chat media API
type DownloadChatRequest struct {
	ChatJID    string   `json:"chat_jid"`
	After      string   `json:"after,omitempty"`       // RFC 3339, inclusive
	Before     string   `json:"before,omitempty"`      // RFC 3339, exclusive
	MediaTypes []string `json:"media_types,omitempty"` // image, video, audio, document
	MediaRetry *bool    `json:"media_retry,omitempty"` // default true
}

// DownloadChatItem is the outcome for one message of a chat download
type DownloadChatItem struct {
	MessageID string          `json:"message_id"`
	Success   bool            `json:"success"`
	Error     string          `json:"error,omitempty"`
	Media     *DownloadResult `json:"media,omitempty"`
}

// DownloadChatResponse represents the response for the download chat media API
type DownloadChatResponse struct {
	Success    bool   `json:"success"`
	Message    string `json:"message"`
	Total      int    `json:"total"`
	Downloaded int    `json:"downloaded"`
	Cached     int    `json:"cached"`
	Failed     int    `json:"failed"`
	// Set when the phone stopped answering re-upload requests and the rest were tried without one
	RetryDisabled bool               `json:"retry_disabled,omitempty"`
	Items         []DownloadChatItem `json:"items"`
}
