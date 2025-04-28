package telegram

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/tg"

	"slices"
	"telegram-obsidian/config"
	"telegram-obsidian/utils"
)

// TelegramManager handles communication with Telegram API
type TelegramManager struct {
	config      *config.Config
	client      *telegram.Client
	api         *tg.Client
	sender      *message.Sender
	entityCache map[string]tg.InputPeerClass
	ctx         context.Context
	ctxCancel   context.CancelFunc
	initialized bool
	background  bool
}

// Custom errors
var (
	ErrTelegramConnection = errors.New("telegram connection error")
	ErrEntityResolution   = errors.New("failed to resolve entity")
	ErrAccessDenied       = errors.New("access denied to entity")
	ErrAuthNeeded         = errors.New("authentication required")
)

// NewTelegramManager creates a new Telegram manager
func NewTelegramManager(cfg *config.Config) *TelegramManager {
	// Create session storage
	sessionPath := filepath.Join(cfg.SessionDir, cfg.SessionName+".json")
	sessionStorage := &session.FileStorage{Path: sessionPath}
	
	// Create client options
	opts := telegram.Options{
		SessionStorage: sessionStorage,
	}
	
	// Create telegram client
	client := telegram.NewClient(cfg.APIID, cfg.APIHash, opts)
	
	return &TelegramManager{
		config:      cfg,
		client:      client,
		entityCache: make(map[string]tg.InputPeerClass),
		initialized: false,
		ctx:         nil,
		ctxCancel:   nil,
		background:  false,
	}
}

// Connect connects to Telegram API
func (tm *TelegramManager) Connect(ctx context.Context) error {
	utils.LogInfo("Connecting to Telegram API...")

	// Reuse the same session file from the main client that already authenticated
	sessionPath := filepath.Join(tm.config.SessionDir, tm.config.SessionName+".json")
	sessionStorage := &session.FileStorage{Path: sessionPath}

	// Create new client with the same credentials
	opts := telegram.Options{
		SessionStorage: sessionStorage,
	}

	tm.client = telegram.NewClient(tm.config.APIID, tm.config.APIHash, opts)

	// Create child context with cancel function
	bgCtx, cancel := context.WithCancel(ctx)
	tm.ctx = bgCtx
	tm.ctxCancel = cancel
	
	// Channel to communicate errors from the background goroutine
	errCh := make(chan error, 1)

	// Run client in background goroutine
	go func() {
		err := tm.client.Run(bgCtx, func(ctx context.Context) error {
			// Get API client
			tm.api = tm.client.API()

			// Check auth status
			status, err := tm.client.Auth().Status(ctx)
			if err != nil {
				errCh <- fmt.Errorf("failed to get auth status: %v", err)
				return err
			}

			if !status.Authorized {
				errCh <- fmt.Errorf("not authorized, please run authentication first")
				return fmt.Errorf("not authorized")
			}

			// Create sender for message operations
			tm.sender = message.NewSender(tm.api)
			
			// Mark as initialized
			tm.initialized = true
			tm.background = true
			
			// Signal success by closing the channel
			close(errCh)

			utils.LogInfo("Successfully connected to Telegram API")
			
			// Keep running until context is cancelled
			<-ctx.Done()
			return nil
		})

		if err != nil && err != context.Canceled {
			utils.LogError("Telegram client closed with error: %v", err)
		}
	}()

	// Wait for initialization or error
	select {
	case err, ok := <-errCh:
		if ok && err != nil {
			tm.ctxCancel()
			return err
		}
	case <-time.After(5 * time.Second):
		tm.ctxCancel()
		return fmt.Errorf("connection timeout after 5 seconds")
	}

	return nil
}

// Disconnect disconnects from Telegram API
func (tm *TelegramManager) Disconnect() error {
	if tm.ctxCancel != nil {
		tm.ctxCancel() // Cancel context to signal client to stop
		tm.initialized = false
		tm.background = false
	}
	
	utils.LogInfo("Disconnected from Telegram API")
	return nil
}

// ResolveEntity resolves an entity by its identifier (ID, username, or link)
func (tm *TelegramManager) ResolveEntity(ctx context.Context, entityIdentifier string) (*tg.InputPeerClass, error) {
	// Check if already in cache
	if peer, ok := tm.entityCache[entityIdentifier]; ok {
		return &peer, nil
	}

	// Try to parse as numeric ID
	if id, err := strconv.ParseInt(entityIdentifier, 10, 64); err == nil {
		return tm.getEntityByID(ctx, id)
	}

	// Try to resolve as username or link
	if strings.HasPrefix(entityIdentifier, "@") {
		// Username format: @username
		username := strings.TrimPrefix(entityIdentifier, "@")
		return tm.getEntityByUsername(ctx, username)
	} else if strings.HasPrefix(entityIdentifier, "https://t.me/") {
		// Link format: https://t.me/username
		parts := strings.Split(entityIdentifier, "/")
		username := parts[len(parts)-1]
		return tm.getEntityByUsername(ctx, username)
	} else if strings.HasPrefix(entityIdentifier, "t.me/") {
		// Link format: t.me/username
		parts := strings.Split(entityIdentifier, "/")
		username := parts[len(parts)-1]
		return tm.getEntityByUsername(ctx, username)
	}

	// Try as plain username (without @)
	return tm.getEntityByUsername(ctx, entityIdentifier)
}

// getEntityByID resolves an entity by its numeric ID
func (tm *TelegramManager) getEntityByID(ctx context.Context, id int64) (*tg.InputPeerClass, error) {
	utils.LogDebug("Resolving entity by ID: %d", id)

	// Check if TelegramManager is initialized
	if !tm.initialized || tm.api == nil {
		return nil, fmt.Errorf("telegram client not initialized or disconnected")
	}

	var peer tg.InputPeerClass

	// Handle "super group/channel" format IDs
	// Telegram channels and supergroups often have IDs like -1001234567890
	// These need to be converted to the actual channel ID
	originalID := id
	if id < 0 {
		// For supergroups and channels which usually have negative IDs
		// In the format -1001234567890, we need to get the real ID
		// This is a common format for Telegram channel IDs
		channelID := id
		if id < -1000000000000 {
			// Supergroups and channels with -100 prefix
			channelID = -id - 1000000000000
			utils.LogDebug("Converting supergroup/channel ID from %d to %d", id, channelID)
		} else if id < -1000000 {
			// Regular group chats with -1000000 (million) prefix
			channelID = -id
			utils.LogDebug("Converting group chat ID from %d to %d", id, channelID)
		}

		// Try as channel/supergroup
		channelPeer := &tg.InputPeerChannel{
			ChannelID:  channelID,
			AccessHash: 0, // We'll try with zero hash first
		}

		// Try to get channel info
		_, err := tm.api.ChannelsGetFullChannel(ctx, &tg.InputChannel{
			ChannelID:  channelID,
			AccessHash: 0,
		})

		if err == nil {
			// It's a channel
			peer = channelPeer
			tm.entityCache[strconv.FormatInt(originalID, 10)] = peer
			return &peer, nil
		}

		// Try with the channels from recent dialogs to get an access hash
		channels, err := tm.GetRecentChannels(ctx, 100)
		if err == nil {
			for _, channel := range channels {
				// Check both regular channel ID and the negative version
				if channel.ID == channelID {
					utils.LogDebug("Found channel in recent dialogs: %s (ID: %d)", channel.Title, channel.ID)
					
					peer = &tg.InputPeerChannel{
						ChannelID:  channelID,
						AccessHash: channel.AccessHash,
					}
					tm.entityCache[strconv.FormatInt(originalID, 10)] = peer
					return &peer, nil
				}
			}
		}
	}

	// First, we'll try to find this entity in our recent dialogs
	// This will give us the access hash if the entity is available
	channels, err := tm.GetRecentChannels(ctx, 100)
	if err == nil {
		for _, channel := range channels {
			if channel.ID == id {
				utils.LogDebug("Found entity in recent dialogs: %s (ID: %d)", channel.Title, id)
				
				switch channel.Type {
				case "user":
					peer = &tg.InputPeerUser{
						UserID:     id,
						AccessHash: channel.AccessHash,
					}
					tm.entityCache[fmt.Sprintf("%d", id)] = peer
					return &peer, nil
				case "channel", "supergroup":
					peer = &tg.InputPeerChannel{
						ChannelID:  id,
						AccessHash: channel.AccessHash,
					}
					tm.entityCache[fmt.Sprintf("%d", id)] = peer
					return &peer, nil
				case "chat":
					peer = &tg.InputPeerChat{
						ChatID: id,
					}
					tm.entityCache[fmt.Sprintf("%d", id)] = peer
					return &peer, nil
				}
			}
		}
	}

	// For user
	userPeer := &tg.InputPeerUser{
		UserID:     id,
		AccessHash: 0, // We'll assume public users initially - for private, we'll need access hash
	}

	// Try getting user info
	_, err = tm.api.UsersGetFullUser(ctx, &tg.InputUser{
		UserID:     id,
		AccessHash: 0,
	})

	if err == nil {
		// It's a user
		peer = userPeer
		tm.entityCache[strconv.FormatInt(id, 10)] = peer
		return &peer, nil
	}

	// For channel/supergroup
	channelPeer := &tg.InputPeerChannel{
		ChannelID:  id,
		AccessHash: 0, // We'll assume public channels initially
	}

	// Try as channel
	_, err = tm.api.ChannelsGetFullChannel(ctx, &tg.InputChannel{
		ChannelID:  id,
		AccessHash: 0,
	})

	if err == nil {
		// It's a channel
		peer = channelPeer
		tm.entityCache[strconv.FormatInt(id, 10)] = peer
		return &peer, nil
	}

	// For chats (groups)
	chatPeer := &tg.InputPeerChat{
		ChatID: id,
	}

	// Try as chat
	_, err = tm.api.MessagesGetFullChat(ctx, id)
	if err == nil {
		// It's a chat
		peer = chatPeer
		tm.entityCache[strconv.FormatInt(id, 10)] = peer
		return &peer, nil
	}

	// If we reach here, we couldn't resolve the entity
	return nil, fmt.Errorf("%w: could not resolve entity with ID %d - it must be in your recent dialogs or be a public entity", ErrEntityResolution, id)
}

// getEntityByUsername resolves an entity by its username
func (tm *TelegramManager) getEntityByUsername(ctx context.Context, username string) (*tg.InputPeerClass, error) {
	utils.LogDebug("Resolving entity by username: %s", username)

	// Use resolveUsername to get entity info
	resolved, err := tm.api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{
		Username: username,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: could not resolve username %s: %v", ErrEntityResolution, username, err)
	}

	var peer tg.InputPeerClass

	// Check the type of the resolved entity
	if len(resolved.Users) > 0 {
		// Extract user from the slice
		user, ok := resolved.Users[0].(*tg.User)
		if !ok {
			return nil, fmt.Errorf("%w: failed to cast user for username %s", ErrEntityResolution, username)
		}

		peer = &tg.InputPeerUser{
			UserID:     user.ID,
			AccessHash: user.AccessHash,
		}
	} else if len(resolved.Chats) > 0 {
		// Extract chat from the slice
		chat, ok := resolved.Chats[0].(*tg.Chat)
		if ok {
			peer = &tg.InputPeerChat{
				ChatID: chat.ID,
			}
		} else {
			// Look for channels in the chats slice
			for _, c := range resolved.Chats {
				if channel, ok := c.(*tg.Channel); ok {
					peer = &tg.InputPeerChannel{
						ChannelID:  channel.ID,
						AccessHash: channel.AccessHash,
					}
					break
				}
			}
		}

		if peer == nil {
			return nil, fmt.Errorf("%w: no valid chat or channel found for username %s", ErrEntityResolution, username)
		}
	} else {
		return nil, fmt.Errorf("%w: no entity found for username %s", ErrEntityResolution, username)
	}

	// Add to cache
	tm.entityCache[username] = peer

	return &peer, nil
}

// GetEntityInfo gets information about an entity
func (tm *TelegramManager) GetEntityInfo(ctx context.Context, peer tg.InputPeerClass) (string, string, error) {
	var title string
	var entityType string

	switch p := peer.(type) {
	case *tg.InputPeerUser:
		users, err := tm.api.UsersGetUsers(ctx, []tg.InputUserClass{
			&tg.InputUser{
				UserID:     p.UserID,
				AccessHash: p.AccessHash,
			},
		})
		if err != nil {
			return "", "", fmt.Errorf("failed to get user info: %v", err)
		}

		if len(users) == 0 {
			return "", "", fmt.Errorf("user not found")
		}

		user := users[0].(*tg.User)
		title = user.FirstName
		if user.LastName != "" {
			title += " " + user.LastName
		}
		entityType = "user"

	case *tg.InputPeerChat:
		fullChat, err := tm.api.MessagesGetFullChat(ctx, p.ChatID)
		if err != nil {
			return "", "", fmt.Errorf("failed to get chat info: %v", err)
		}

		for _, chat := range fullChat.Chats {
			if chat.GetID() == p.ChatID {
				title = chat.(*tg.Chat).Title
				entityType = "chat"
				break
			}
		}

	case *tg.InputPeerChannel:
		channels, err := tm.api.ChannelsGetChannels(ctx, []tg.InputChannelClass{
			&tg.InputChannel{
				ChannelID:  p.ChannelID,
				AccessHash: p.AccessHash,
			},
		})
		if err != nil {
			return "", "", fmt.Errorf("failed to get channel info: %v", err)
		}

		for _, ch := range channels.GetChats() {
			channel, ok := ch.(*tg.Channel)
			if ok && channel.ID == p.ChannelID {
				title = channel.Title
				if channel.Megagroup || channel.Gigagroup {
					entityType = "supergroup"
				} else {
					entityType = "channel"
				}
				break
			}
		}
	}

	return title, entityType, nil
}

// Message represents a Telegram message
type Message struct {
	ID         int64
	EntityID   string
	Date       time.Time
	Text       string
	ReplyToID  int64
	GroupedID  int64
	MediaItems []*MediaItem
}

// MediaItem represents a media item in a message
type MediaItem struct {
	Type       string // photo, video, audio, document
	FileID     string
	AccessHash int64
	FileSize   int64
	MimeType   string
	Extension  string
	Width      int
	Height     int
	Duration   int
}

// FetchMessages fetches messages from an entity
func (tm *TelegramManager) FetchMessages(ctx context.Context, peer tg.InputPeerClass, minID int64, batchSize int) ([]*Message, error) {
	utils.LogDebug("Fetching messages from entity, minID: %d, batchSize: %d", minID, batchSize)

	// Create messages request
	request := &tg.MessagesGetHistoryRequest{
		Peer:       peer,
		Limit:      batchSize,
		MinID:      int(minID), // Convert int64 to int as required by API
		OffsetID:   0,
		OffsetDate: 0,
		AddOffset:  0,
		MaxID:      0,
		Hash:       0,
	}

	// Get messages from API
	history, err := tm.api.MessagesGetHistory(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("failed to get messages: %v", err)
	}

	// Convert to Message objects
	messages := make([]*Message, 0)

	// Extract messages from the response based on its type
	var msgs []tg.MessageClass

	switch h := history.(type) {
	case *tg.MessagesChannelMessages:
		msgs = h.Messages
	case *tg.MessagesMessages:
		msgs = h.Messages
	case *tg.MessagesMessagesSlice:
		msgs = h.Messages
	default:
		return nil, fmt.Errorf("unknown messages response type: %T", history)
	}

	// Loop through messages
	for _, m := range msgs {
		// Skip service messages
		if _, ok := m.(*tg.MessageService); ok {
			continue
		}

		msg, ok := m.(*tg.Message)
		if !ok {
			continue
		}

		// Create message object
		message := &Message{
			ID:         int64(msg.ID),
			EntityID:   getPeerID(msg.PeerID),
			Date:       time.Unix(int64(msg.Date), 0),
			Text:       msg.Message,
			ReplyToID:  0,
			GroupedID:  msg.GroupedID,
			MediaItems: make([]*MediaItem, 0),
		}

		// Set reply to ID if exists
		if msg.ReplyTo != nil {
			reply, ok := msg.ReplyTo.(*tg.MessageReplyHeader)
			if ok {
				message.ReplyToID = int64(reply.ReplyToMsgID)
			}
		}
		// Process media
		if msg.Media != nil {
			// Extract media information
			mediaItems := tm.extractMediaFromMessage(msg)
			if len(mediaItems) > 0 {
				message.MediaItems = mediaItems
			}
		}

		messages = append(messages, message)
	}
	utils.LogInfo("Fetched %d messages", len(messages))
	return messages, nil
}

// extractMediaFromMessage extracts media items from a message
func (tm *TelegramManager) extractMediaFromMessage(msg *tg.Message) []*MediaItem {
	mediaItems := make([]*MediaItem, 0)
	if msg.Media == nil {
		return mediaItems
	}
	switch media := msg.Media.(type) {
	case *tg.MessageMediaPhoto:
		if photo, ok := media.Photo.(*tg.Photo); ok {
			// Find the largest photo size
			var largest *tg.PhotoSize
			for _, size := range photo.Sizes {
				if photoSize, ok := size.(*tg.PhotoSize); ok {
					if largest == nil || (photoSize.W > largest.W && photoSize.H > largest.H) {
						largest = photoSize
					}
				}
			}
			if largest != nil {
				mediaItems = append(mediaItems, &MediaItem{
					Type:       "photo",
					FileID:     fmt.Sprintf("%d", photo.ID),
					AccessHash: photo.AccessHash,
					FileSize:   int64(largest.Size),
					Width:      largest.W,
					Height:     largest.H,
					Extension:  "jpg", // Photos are typically JPEG
				})
			}
		}
	case *tg.MessageMediaDocument:
		if doc, ok := media.Document.(*tg.Document); ok {
			// Determine media type from attributes
			mediaType := "document"
			var width, height int
			var duration int
			var mimeType, extension string
			mimeType = doc.MimeType

			// Try to determine file extension from MIME type
			extension = getExtensionFromMimeType(mimeType)
			// Check attributes for more info
			for _, attr := range doc.Attributes {
				switch a := attr.(type) {
				case *tg.DocumentAttributeVideo:
					mediaType = "video"
					width = a.W
					height = a.H
					duration = int(a.Duration) // Convert float64 to int
				case *tg.DocumentAttributeAudio:
					if a.Voice {
						mediaType = "voice"
					} else {
						mediaType = "audio"
					}
					duration = int(a.Duration) // Convert float64 to int
				case *tg.DocumentAttributeFilename:
					// Get extension from filename if available
					parts := strings.Split(a.FileName, ".")
					if len(parts) > 1 {
						extension = parts[len(parts)-1]
					}
				}
			}
			mediaItems = append(mediaItems, &MediaItem{
				Type:       mediaType,
				FileID:     fmt.Sprintf("%d", doc.ID),
				AccessHash: doc.AccessHash,
				FileSize:   doc.Size,
				MimeType:   mimeType,
				Extension:  extension,
				Width:      width,
				Height:     height,
				Duration:   duration,
			})
		}
	}
	return mediaItems
}

// DownloadMedia downloads media from Telegram
func (tm *TelegramManager) DownloadMedia(ctx context.Context, mediaItem *MediaItem, outputPath string) error {
	utils.LogInfo("Downloading media to %s", outputPath)

	// Ensure parent directory exists
	err := utils.EnsureDirExists(filepath.Dir(outputPath))
	if err != nil {
		return fmt.Errorf("failed to create directory: %v", err)
	}

	// Check if file already exists
	if _, err := os.Stat(outputPath); err == nil {
		utils.LogDebug("File already exists, skipping download: %s", outputPath)
		return nil
	}

	// Create temporary file for download
	tempPath := outputPath + ".tmp"

	// Different ways to download based on media type
	var downloadErr error

	switch mediaItem.Type {
	case "photo":
		// For photos, we need to create an InputPhotoFileLocation
		// Parse FileID string to int64
		photoID, err := strconv.ParseInt(mediaItem.FileID, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid photo ID: %v", err)
		}

		inputLocation := &tg.InputPhotoFileLocation{
			ID:            photoID,
			AccessHash:    mediaItem.AccessHash,
			FileReference: []byte{}, // This might need to be populated
			ThumbSize:     "w",      // Get the largest size
		}

		downloadErr = tm.downloadFile(ctx, inputLocation, mediaItem.FileSize, tempPath)

	case "video", "audio", "voice", "document", "animation":
		// Parse FileID string to int64
		docID, err := strconv.ParseInt(mediaItem.FileID, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid document ID: %v", err)
		}

		// For documents (which includes videos, audios, etc)
		inputLocation := &tg.InputDocumentFileLocation{
			ID:            docID,
			AccessHash:    mediaItem.AccessHash,
			FileReference: []byte{},
			ThumbSize:     "", // No thumbnail for document
		}

		downloadErr = tm.downloadFile(ctx, inputLocation, mediaItem.FileSize, tempPath)

	default:
		downloadErr = fmt.Errorf("unsupported media type: %s", mediaItem.Type)
	}

	// Check if download was successful
	if downloadErr != nil {
		// Clean up temporary file if it exists
		os.Remove(tempPath)
		return fmt.Errorf("failed to download media: %v", downloadErr)
	}

	// Rename temporary file to final path
	if err := os.Rename(tempPath, outputPath); err != nil {
		os.Remove(tempPath) // Clean up temporary file
		return fmt.Errorf("failed to move temporary file to final path: %v", err)
	}

	utils.LogInfo("Media download completed: %s", outputPath)
	return nil
}

// downloadFile downloads a file from Telegram using the gotd/td API
func (tm *TelegramManager) downloadFile(ctx context.Context, location tg.InputFileLocationClass, fileSize int64, outputPath string) error {
	// Open output file
	file, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %v", err)
	}
	defer file.Close()
	//                Determine appropriate chunk size: 512KB for most files
	chunkSize := 512 * 1024 // 512 KB chunks
	// Larger chunks for big files
	if fileSize > 10*1024*1024 { // 10 MB
		chunkSize = 1024 * 1024 // 1 MB chunks
	}
	// Limit maximum chunk size as per Telegram API limit
	if chunkSize > 1024*1024 {
		chunkSize = 1024 * 1024 // Maximum 1 MB
	}
	// Calculate total chunks
	totalChunks := (fileSize + int64(chunkSize) - 1) / int64(chunkSize)
	if totalChunks == 0 {
		totalChunks = 1 // At least one chunk
	}

	utils.LogDebug("Downloading file with %d chunks of %d bytes each", totalChunks, chunkSize)
	// Download in chunks
	var offset int64
	var bytesDownloaded int64
	var lastProgressLog time.Time = time.Now()
	for chunk := int64(0); chunk < totalChunks; chunk++ {
		// Add a small delay to avoid hitting rate limits
		if chunk > 0 {
			time.Sleep(tm.config.RequestDelay)
		}
		// Download chunk
		req := &tg.UploadGetFileRequest{
			Location: location,
			Offset:   offset,
			Limit:    chunkSize,
		}

		res, err := tm.api.UploadGetFile(ctx, req)
		if err != nil {
			return fmt.Errorf("failed to download chunk %d: %v", chunk, err)
		}
		// Extract bytes from response
		var bytes []byte
		switch r := res.(type) {
		case *tg.UploadFile:
			bytes = r.Bytes
		default:
			return fmt.Errorf("unknown response type: %T", res)
		}
		// Write to file
		bytesWritten, err := file.Write(bytes)
		if err != nil {
			return fmt.Errorf("failed to write chunk to file: %v", err)
		}

		// Update offset for next chunk
		offset += int64(bytesWritten)
		bytesDownloaded += int64(bytesWritten)

		// Log progress for large files every 2 seconds
		if (time.Since(lastProgressLog) > 2*time.Second || chunk == totalChunks-1) && fileSize > 1024*1024 {
			progress := float64(bytesDownloaded) / float64(fileSize) * 100
			utils.LogDebug("Download progress: %.1f%% (%.2f MB / %.2f MB)",
				progress,
				float64(bytesDownloaded)/(1024*1024),
				float64(fileSize)/(1024*1024))
			lastProgressLog = time.Now()
		}

		// If we got fewer bytes than requested, we're done
		if len(bytes) < chunkSize {
			break
		}
	}

	// Perform additional validation
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to get file stats: %v", err)
	}
	// Check if we downloaded the expected amount of data
	if fileSize > 0 && info.Size() < fileSize {
		utils.LogWarn("Downloaded file size (%d bytes) is less than expected (%d bytes)", info.Size(), fileSize)
	}

	return nil
}

// ChannelInfo represents basic channel information
type ChannelInfo struct {
	ID          int64
	AccessHash  int64
	Title       string
	Username    string
	Type        string //
	MemberCount int
}

// GetRecentChannels gets a list of recent channels/dialogs
func (tm *TelegramManager) GetRecentChannels(ctx context.Context, limit int) ([]ChannelInfo, error) {
	utils.LogInfo("Fetching recent channels/dialogs, limit: %d", limit)

	// Check if TelegramManager is initialized
	if !tm.initialized || tm.api == nil {
		return nil, fmt.Errorf("telegram client not initialized or disconnected")
	}

	// Use the saved context if the context passed is nil
	if ctx == nil {
		ctx = tm.ctx
	}

	// Use a timeout context to avoid hanging
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Get dialogs
	input := &tg.MessagesGetDialogsRequest{
		OffsetDate: 0,
		OffsetID:   0,
		OffsetPeer: &tg.InputPeerEmpty{},
		Limit:      limit,
		Hash:       0,
	}

	// Get dialogs directly from API
	dialogs, err := tm.api.MessagesGetDialogs(timeoutCtx, input)
	if err != nil {
		utils.LogError("Failed to get dialogs: %v", err)
		return nil, fmt.Errorf("failed to get dialogs: %v", err)
	}

	channels := make([]ChannelInfo, 0)

	// Process different response types
	var chats []tg.ChatClass
	var users []tg.UserClass
	switch d := dialogs.(type) {
	case *tg.MessagesDialogs:
		chats = d.Chats
		users = d.Users
	case *tg.MessagesDialogsSlice:
		chats = d.Chats
		users = d.Users
	default:
		return nil, fmt.Errorf("unknown dialogs response type: %T", dialogs)
	}

	// Extract channels from chats
	for _, chat := range chats {
		switch c := chat.(type) {
		case *tg.Channel:
			channelType := "channel"
			if c.Megagroup || c.Gigagroup {
				channelType = "supergroup"
			}

			channels = append(channels, ChannelInfo{
				ID:          c.ID,
				AccessHash:  c.AccessHash,
				Title:       c.Title,
				Username:    c.Username,
				Type:        channelType,
				MemberCount: c.ParticipantsCount,
			})
		case *tg.Chat:
			channels = append(channels, ChannelInfo{
				ID:          c.ID,
				Title:       c.Title,
				Type:        "chat",
				MemberCount: c.ParticipantsCount,
			})
		}
	}
	
	// Process users separately (they're also dialogs but in a different list)
	for _, userObj := range users {
		if u, ok := userObj.(*tg.User); ok && !u.Bot {
			// Add private chats with users too (but not bots)
			fullName := u.FirstName
			if u.LastName != "" {
				fullName += " " + u.LastName
			}

			channels = append(channels, ChannelInfo{
				ID:         u.ID,
				AccessHash: u.AccessHash,
				Title:      fullName,
				Username:   u.Username,
				Type:       "user",
			})
		}
	}

	utils.LogInfo("Fetched %d channels/dialogs", len(channels))
	return channels, nil
}

// RunInteractiveSelection runs interactive mode for selecting export targets
func (tm *TelegramManager) RunInteractiveSelection(ctx context.Context) error {
	if !tm.config.InteractiveMode {
		return nil
	}

	utils.LogInfo("Running interactive selection mode")

	reader := bufio.NewReader(os.Stdin)
	var selectedTargets []config.ExportTarget

	for {
		fmt.Println("\n-------- Telegram Export Menu --------")
		fmt.Println("1. List and select from recent chats/channels")
		fmt.Println("2. Enter chat/channel ID or username manually")
		if len(selectedTargets) > 0 {
			fmt.Println("3. View selected targets")
			fmt.Println("4. Start export with selected targets")
		}
		fmt.Println("0. Exit without exporting")
		fmt.Print("\nEnter your choice (0-4): ")

		choiceStr, _ := reader.ReadString('\n')
		choiceStr = strings.TrimSpace(choiceStr)
		choice, err := strconv.Atoi(choiceStr)
		if err != nil {
			fmt.Println("Invalid choice. Please enter a number.")
			continue
		}

		switch choice {
		case 0:
			return fmt.Errorf("user chose to exit without exporting")

		case 1:
			// List and select from recent chats/channels
			channels, err := tm.GetRecentChannels(ctx, 20)
			if err != nil {
				fmt.Printf("Error fetching channels: %v\n", err)
				continue
			}

			target, err := tm.selectFromChannelList(reader, channels)
			if err != nil {
				fmt.Printf("Error selecting channel: %v\n", err)
				continue
			}

			if target != nil {
				selectedTargets = append(selectedTargets, *target)
				fmt.Printf("Added %s to export targets\n", target.Name)
			}

		case 2:
			// Enter channel ID or username manually
			target, err := tm.enterManualTarget(ctx, reader)
			if err != nil {
				fmt.Printf("Error adding target: %v\n", err)
				continue
			}

			if target != nil {
				selectedTargets = append(selectedTargets, *target)
				fmt.Printf("Added %s to export targets\n", target.Name)
			}

		case 3:
			// View selected targets
			if len(selectedTargets) == 0 {
				fmt.Println("No targets selected yet.")
				continue
			}

			fmt.Println("\nSelected Export Targets:")
			for i, target := range selectedTargets {
				fmt.Printf("%d. %s (%s) - ID: %s\n", i+1, target.Name, target.Type, target.ID)
			}

			// Option to remove a target
			fmt.Print("\nEnter number to remove (or 0 to go back): ")
			indexStr, _ := reader.ReadString('\n')
			indexStr = strings.TrimSpace(indexStr)
			index, err := strconv.Atoi(indexStr)
			if err != nil || index < 0 || index > len(selectedTargets) {
				continue
			}

			if index > 0 {
				// Remove the selected target
				removedTarget := selectedTargets[index-1]
				selectedTargets = slices.Delete(selectedTargets, index-1, index)
				fmt.Printf("Removed %s from export targets\n", removedTarget.Name)
			}

		case 4:
			// Start export with selected targets
			if len(selectedTargets) == 0 {
				fmt.Println("No targets selected yet.")
				continue
			}

			fmt.Printf("Starting export with %d selected targets...\n", len(selectedTargets))
			// Clear existing export targets and add the selected ones
			tm.config.ExportTargets = []config.ExportTarget{}
			for _, target := range selectedTargets {
				tm.config.AddExportTarget(target)
			}

			return nil

		default:
			fmt.Println("Invalid choice. Please try again.")
		}
	}
}

// selectFromChannelList displays a list of channels and lets the user select one
func (tm *TelegramManager) selectFromChannelList(reader *bufio.Reader, channels []ChannelInfo) (*config.ExportTarget, error) {
	if len(channels) == 0 {
		fmt.Println("\nNo recent channels found. Try entering a chat ID or username manually.")
		fmt.Println("Note: Make sure you've opened the channel/chat recently in your Telegram app.")
		return nil, nil
	}

	fmt.Println("\nRecent Chats/Channels:")
	for i, channel := range channels {
		memberInfo := ""
		if channel.MemberCount > 0 {
			memberInfo = fmt.Sprintf(" - %d members", channel.MemberCount)
		}
		fmt.Printf("%d. %s (%s)%s\n", i+1, channel.Title, channel.Type, memberInfo)
	}

	fmt.Print("\nSelect chat/channel (or 0 to go back): ")
	indexStr, _ := reader.ReadString('\n')
	indexStr = strings.TrimSpace(indexStr)
	index, err := strconv.Atoi(indexStr)
	if err != nil || index < 0 || index > len(channels) {
		fmt.Println("Invalid selection.")
		return nil, nil
	}

	if index == 0 {
		return nil, nil
	}

	selected := channels[index-1]
	// Create ExportTarget
	target := &config.ExportTarget{
		ID:   fmt.Sprintf("%d", selected.ID),
		Name: selected.Title,
		Type: selected.Type,
	}
	return target, nil
}

// enterManualTarget allows the user to enter a chat/channel ID or username manually
func (tm *TelegramManager) enterManualTarget(ctx context.Context, reader *bufio.Reader) (*config.ExportTarget, error) {
	fmt.Print("\nEnter channel ID, username (@username), or link (https://t.me/...): ")
	identifier, _ := reader.ReadString('\n')
	identifier = strings.TrimSpace(identifier)

	if identifier == "" {
		return nil, nil
	}

	// Special handling for channel/supergroup IDs
	if id, err := strconv.ParseInt(identifier, 10, 64); err == nil {
		// Handle regular format ID
		if id > 0 {
			fmt.Println("\nNote: When using positive numeric IDs, the chat must be in your recent dialogs or be a public entity.")
			fmt.Println("For channels and supergroups, you might need to use a negative ID format (-100XXXXXXXXX).")
		} else {
			// Negative ID is likely a channel/supergroup
			fmt.Println("\nAttempting to resolve channel/supergroup with negative ID...")
		}
	} else if strings.HasPrefix(identifier, "-") && !strings.HasPrefix(identifier, "-100") {
		// Attempt to add -100 prefix if it's missing for supergroups/channels
		trimmed := strings.TrimPrefix(identifier, "-")
		if _, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			updatedID := "-100" + trimmed
			fmt.Printf("\nConverting ID format to %s for potential supergroup/channel format...\n", updatedID)
			identifier = updatedID
		}
	}

	// Resolve the entity
	peer, err := tm.ResolveEntity(ctx, identifier)
	if err != nil {
		// Provide more helpful error message
		if strings.Contains(err.Error(), "could not resolve entity with ID") {
			fmt.Println("\nCould not find this ID. Make sure:")
			fmt.Println("1. You have recently opened this chat in your Telegram app")
			fmt.Println("2. The chat/channel is public or you are a member")
			fmt.Println("3. For channels/supergroups, try format -100XXXXXXXXX")
			fmt.Println("4. Try using @username format instead of numeric ID")
		} else {
			fmt.Println("\nError:", err)
		}
		return nil, fmt.Errorf("could not resolve '%s': %v", identifier, err)
	}

	// Get entity info
	title, entityType, err := tm.GetEntityInfo(ctx, *peer)
	if err != nil {
		fmt.Println("\nError getting entity information. Make sure you have access to this chat/channel.")
		return nil, fmt.Errorf("could not get info for '%s': %v", identifier, err)
	}

	// Show info and ask for confirmation
	fmt.Printf("\nFound: %s (%s)\n", title, entityType)
	fmt.Print("Add to export targets? (y/n): ")
	confirm, _ := reader.ReadString('\n')
	confirm = strings.TrimSpace(strings.ToLower(confirm))

	if confirm != "y" && confirm != "yes" {
		fmt.Println("Not adding to export targets.")
		return nil, nil
	}

	// Create ExportTarget
	target := &config.ExportTarget{
		ID:   identifier,
		Name: title,
		Type: entityType,
	}

	return target, nil
}

// Helper functions

// getPeerID returns a string representation of a peer ID
func getPeerID(peerID tg.PeerClass) string {
	switch p := peerID.(type) {
	case *tg.PeerUser:
		return fmt.Sprintf("%d", p.UserID)
	case *tg.PeerChat:
		return fmt.Sprintf("%d", p.ChatID)
	case *tg.PeerChannel:
		return fmt.Sprintf("%d", p.ChannelID)
	default:
		return "unknown"
	}
}

// getExtensionFromMimeType tries to determine file extension from MIME type
func getExtensionFromMimeType(mimeType string) string {
	mimeToExt := map[string]string{
		"image/jpeg":                   "jpg",
		"image/png":                    "png",
		"image/gif":                    "gif",
		"image/webp":                   "webp",
		"video/mp4":                    "mp4",
		"video/quicktime":              "mov",
		"video/webm":                   "webm",
		"audio/mpeg":                   "mp3",
		"audio/mp4":                    "m4a",
		"audio/ogg":                    "ogg",
		"audio/x-wav":                  "wav",
		"application/pdf":              "pdf",
		"application/zip":              "zip",
		"application/x-7z-compressed":  "7z",
		"application/x-tar":            "tar",
		"application/x-rar-compressed": "rar",
		"text/plain":                   "txt",
	}
	if ext, ok := mimeToExt[mimeType]; ok {
		return ext
	}
	// For unknown MIME types, try to parse it
	parts := strings.Split(mimeType, "/")
	if len(parts) == 2 {
		return parts[1]
	}
	return "bin"
}
