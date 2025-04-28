package cache

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"telegram-obsidian/notes"
)

// EntityCache represents cache data for a single entity (chat/channel/user)
type EntityCache struct {
	Title        string                 `json:"title"`
	EntityType   string                 `json:"entity_type"`
	LastID       int64                  `json:"last_id"`
	ProcessedMap map[int64]MessageCache `json:"processed_map"`
}

// MessageCache represents cache data for a single processed message
type MessageCache struct {
	NoteFilename string `json:"note_filename"`
	ReplyToID    int64  `json:"reply_to_id,omitempty"`
}

// CacheManager handles caching of exported messages
type CacheManager struct {
	cachePath     string
	cache         map[string]*EntityCache // Maps entity_id -> EntityCache
	mutex         sync.RWMutex
	dirty         bool
	saveScheduled bool
	saveMutex     sync.Mutex
}

// NewCacheManager creates a new cache manager instance
func NewCacheManager(cachePath string) *CacheManager {
	return &CacheManager{
		cachePath:     cachePath,
		cache:         make(map[string]*EntityCache),
		mutex:         sync.RWMutex{},
		dirty:         false,
		saveScheduled: false,
		saveMutex:     sync.Mutex{},
	}
}

// LoadCache loads cache data from file
func (cm *CacheManager) LoadCache() error {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	// Check if cache file exists
	_, err := os.Stat(cm.cachePath)
	if os.IsNotExist(err) {
		fmt.Printf("Cache file does not exist at '%s', starting with empty cache\n", cm.cachePath)
		cm.cache = make(map[string]*EntityCache)
		return nil
	}

	// Read cache file
	data, err := os.ReadFile(cm.cachePath)
	if err != nil {
		return fmt.Errorf("failed to read cache file: %v", err)
	}

	// Handle empty file
	if len(data) == 0 {
		fmt.Println("Cache file is empty, starting with empty cache")
		cm.cache = make(map[string]*EntityCache)
		return nil
	}

	// Parse JSON
	var cache map[string]*EntityCache
	err = json.Unmarshal(data, &cache)
	if err != nil {
		return fmt.Errorf("failed to parse cache file: %v", err)
	}

	// Ensure all entities have necessary fields
	for entityID, entityCache := range cache {
		if entityCache.ProcessedMap == nil {
			entityCache.ProcessedMap = make(map[int64]MessageCache)
		}
		cache[entityID] = entityCache
	}

	cm.cache = cache
	return nil
}

// SaveCache saves cache data to file
func (cm *CacheManager) SaveCache() error {
	cm.mutex.RLock()
	if !cm.dirty {
		cm.mutex.RUnlock()
		return nil
	}

	// Make a copy of the cache to release the read lock quickly
	cacheCopy := make(map[string]*EntityCache)
	maps.Copy(cacheCopy, cm.cache)
	cm.mutex.RUnlock()

	// Marshal to JSON
	data, err := json.MarshalIndent(cacheCopy, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal cache: %v", err)
	}

	// Create temporary file
	dir := filepath.Dir(cm.cachePath)
	err = os.MkdirAll(dir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create directory for cache: %v", err)
	}

	tempFile := cm.cachePath + ".tmp"
	err = os.WriteFile(tempFile, data, 0644)
	if err != nil {
		return fmt.Errorf("failed to write temporary cache file: %v", err)
	}

	// Rename temporary file to actual file for atomic update
	err = os.Rename(tempFile, cm.cachePath)
	if err != nil {
		return fmt.Errorf("failed to rename temporary cache file: %v", err)
	}

	cm.mutex.Lock()
	cm.dirty = false
	cm.mutex.Unlock()

	return nil
}

// ScheduleBackgroundSave schedules a background save if one isn't already scheduled
func (cm *CacheManager) ScheduleBackgroundSave() {
	cm.saveMutex.Lock()
	defer cm.saveMutex.Unlock()

	if cm.saveScheduled {
		return
	}

	cm.saveScheduled = true
	go func() {
		// Add a small delay to prevent frequent saves
		time.Sleep(5 * time.Second)

		err := cm.SaveCache()
		if err != nil {
			fmt.Printf("Error saving cache in background: %v\n", err)
		}

		cm.saveMutex.Lock()
		cm.saveScheduled = false
		cm.saveMutex.Unlock()
	}()
}

// IsProcessed checks if a message has been processed
func (cm *CacheManager) IsProcessed(messageID int64, entityID string) bool {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	entityCache, exists := cm.cache[entityID]
	if !exists {
		return false
	}

	_, processed := entityCache.ProcessedMap[messageID]
	return processed
}

// AddProcessedMessage adds a processed message to the cache
func (cm *CacheManager) AddProcessedMessage(messageID int64, noteFilename string, replyToID int64, entityID string) {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	// Ensure entity exists in cache
	entityCache, exists := cm.cache[entityID]
	if !exists {
		entityCache = &EntityCache{
			ProcessedMap: make(map[int64]MessageCache),
			Title:        "",
			EntityType:   "",
			LastID:       0,
		}
		cm.cache[entityID] = entityCache
	}

	// Add message to processed map
	entityCache.ProcessedMap[messageID] = MessageCache{
		NoteFilename: noteFilename,
		ReplyToID:    replyToID,
	}

	// Update last processed ID if this message ID is greater
	if messageID > entityCache.LastID {
		entityCache.LastID = messageID
	}

	cm.dirty = true
}

// UpdateEntityInfo updates the title and type of an entity in the cache
func (cm *CacheManager) UpdateEntityInfo(entityID string, title string, entityType string) {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	// Ensure entity exists in cache
	entityCache, exists := cm.cache[entityID]
	if !exists {
		entityCache = &EntityCache{
			ProcessedMap: make(map[int64]MessageCache),
			Title:        title,
			EntityType:   entityType,
			LastID:       0,
		}
		cm.cache[entityID] = entityCache
	}

	// Update entity info
	entityCache.Title = title
	entityCache.EntityType = entityType

	cm.dirty = true
}

// GetNoteFilename returns the note filename for a message
func (cm *CacheManager) GetNoteFilename(messageID int64, entityID string) string {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	entityCache, exists := cm.cache[entityID]
	if !exists {
		return ""
	}

	messageCache, exists := entityCache.ProcessedMap[messageID]
	if !exists {
		return ""
	}

	return messageCache.NoteFilename
}

// GetAllProcessedMessages returns all processed messages for an entity
func (cm *CacheManager) GetAllProcessedMessages(entityID string) map[int64]notes.MessageCache {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	entityCache, exists := cm.cache[entityID]
	if !exists {
		return nil
	}

	// Convert our internal MessageCache type to notes.MessageCache
	result := make(map[int64]notes.MessageCache)
	for id, msgCache := range entityCache.ProcessedMap {
		result[id] = notes.MessageCache{
			NoteFilename: msgCache.NoteFilename,
			ReplyToID:    msgCache.ReplyToID,
		}
	}

	return result
}

// GetEntityStats returns statistics for all entities
func (cm *CacheManager) GetEntityStats() map[string]map[string]any {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	stats := make(map[string]map[string]any)
	for entityID, entityCache := range cm.cache {
		entityStats := make(map[string]any)
		entityStats["title"] = entityCache.Title
		entityStats["type"] = entityCache.EntityType
		entityStats["message_count"] = len(entityCache.ProcessedMap)
		entityStats["last_id"] = entityCache.LastID

		stats[entityID] = entityStats
	}

	return stats
}

// GetLastProcessedMessageID returns the last processed message ID for an entity
func (cm *CacheManager) GetLastProcessedMessageID(entityID string) int64 {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	entityCache, exists := cm.cache[entityID]
	if !exists {
		return 0
	}

	return entityCache.LastID
}
