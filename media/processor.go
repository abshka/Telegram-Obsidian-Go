package media

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"telegram-obsidian/config"
	"telegram-obsidian/telegram"
	"telegram-obsidian/utils"
)

// MediaProcessor handles downloading and optimizing media files
type MediaProcessor struct {
	config            *config.Config
	telegramManager   *telegram.TelegramManager
	concurrentLimit   chan struct{}
	processedCache    map[string]string
	processedCacheMux sync.RWMutex
	ffmpegAvailable   bool
}

// NewMediaProcessor creates a new media processor
func NewMediaProcessor(cfg *config.Config, tm *telegram.TelegramManager) *MediaProcessor {
	// Check if ffmpeg is available once at startup
	ffmpegAvailable := true
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		utils.LogWarn("FFmpeg not found. Media optimization will be disabled.")
		ffmpegAvailable = false
	}
	
	return &MediaProcessor{
		config:          cfg,
		telegramManager: tm,
		concurrentLimit: make(chan struct{}, cfg.ConcurrentDownloads),
		processedCache:  make(map[string]string),
		ffmpegAvailable: ffmpegAvailable,
	}
}

// DownloadAndOptimizeMedia downloads and optimizes media from a message
func (mp *MediaProcessor) DownloadAndOptimizeMedia(ctx context.Context, message *telegram.Message, entityMediaPath string) ([]string, error) {
	if !mp.config.MediaDownload || len(message.MediaItems) == 0 {
		utils.LogDebug("Skipping media download (disabled in config or no media items)")
		return nil, nil
	}

	utils.LogInfo("Processing %d media items for message %d", len(message.MediaItems), message.ID)

	var wg sync.WaitGroup
	mediaPathsChan := make(chan string, len(message.MediaItems))
	errorsChan := make(chan error, len(message.MediaItems))

	for _, mediaItem := range message.MediaItems {
		wg.Add(1)
		go func(item *telegram.MediaItem) {
			defer wg.Done()

			// Acquire semaphore
			mp.concurrentLimit <- struct{}{}
			defer func() { <-mp.concurrentLimit }()

			path, err := mp.processSingleItem(ctx, message.ID, message.EntityID, item, entityMediaPath)
			if err != nil {
				errorsChan <- err
				return
			}

			if path != "" {
				mediaPathsChan <- path
			}
		}(mediaItem)
	}

	wg.Wait()
	close(mediaPathsChan)
	close(errorsChan)

	// Collect paths and errors
	var mediaPaths []string
	for path := range mediaPathsChan {
		mediaPaths = append(mediaPaths, path)
	}

	var errs []error
	for err := range errorsChan {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return mediaPaths, fmt.Errorf("errors occurred while processing media: %v", errs[0])
	}

	return mediaPaths, nil
}

// processSingleItem processes a single media item
func (mp *MediaProcessor) processSingleItem(ctx context.Context, messageID int64, entityID string, mediaItem *telegram.MediaItem, entityMediaPath string) (string, error) {
	// Generate a filename for the media
	filename := mp.getFilename(mediaItem, messageID, entityID)
	cacheKey := fmt.Sprintf("%s_%s_%s", entityID, mediaItem.FileID, mediaItem.Type)

	// Check if we've already processed this media
	mp.processedCacheMux.RLock()
	if cachedPath, exists := mp.processedCache[cacheKey]; exists {
		mp.processedCacheMux.RUnlock()
		utils.LogDebug("Using cached media for %s: %s", cacheKey, cachedPath)
		return cachedPath, nil
	}
	mp.processedCacheMux.RUnlock()

	// Create paths
	rawDownloadPath := filepath.Join(entityMediaPath, "raw_"+filename)
	finalPath := filepath.Join(entityMediaPath, filename)

	// Check if final file already exists
	if _, err := os.Stat(finalPath); err == nil {
		// File already exists, add to cache and return
		mp.processedCacheMux.Lock()
		mp.processedCache[cacheKey] = finalPath
		mp.processedCacheMux.Unlock()
		utils.LogDebug("Final media file already exists: %s", finalPath)
		return finalPath, nil
	}

	// Create media directory if it doesn't exist
	err := utils.EnsureDirExists(entityMediaPath)
	if err != nil {
		return "", fmt.Errorf("failed to create media directory: %v", err)
	}

	// Download the media
	err = mp.telegramManager.DownloadMedia(ctx, mediaItem, rawDownloadPath)
	if err != nil {
		return "", fmt.Errorf("failed to download media: %v", err)
	}

	// Get file info for logging
	fileInfo, err := os.Stat(rawDownloadPath)
	if err == nil {
		utils.LogInfo("Downloaded %s media (%s): %d bytes", 
			mediaItem.Type, 
			rawDownloadPath, 
			fileInfo.Size())
	}

	// Optimize the media
	optimizeStart := time.Now()
	err = mp.optimizeMedia(rawDownloadPath, finalPath, mediaItem)
	if err != nil {
		utils.LogWarn("Failed to optimize media, using raw file: %v", err)
		// If optimization fails, just use the raw file
		err = os.Rename(rawDownloadPath, finalPath)
		if err != nil {
			return "", fmt.Errorf("failed to rename raw media file: %v", err)
		}
	} else {
		// Log optimization results
		if originalInfo, err := os.Stat(rawDownloadPath); err == nil {
			if optimizedInfo, err := os.Stat(finalPath); err == nil {
				originalSize := originalInfo.Size()
				optimizedSize := optimizedInfo.Size()
				savings := float64(originalSize-optimizedSize) / float64(originalSize) * 100
				utils.LogInfo("Media optimization: %s reduced from %d to %d bytes (%.1f%% savings) in %.2f seconds",
					mediaItem.Type,
					originalSize,
					optimizedSize,
					savings,
					time.Since(optimizeStart).Seconds())
			}
		}

		// Clean up the raw file
		os.Remove(rawDownloadPath)
	}

	// Add to cache
	mp.processedCacheMux.Lock()
	mp.processedCache[cacheKey] = finalPath
	mp.processedCacheMux.Unlock()

	return finalPath, nil
}

// getFilename generates a filename for a media item
func (mp *MediaProcessor) getFilename(mediaItem *telegram.MediaItem, messageID int64, entityID string) string {
	// Create a unique filename based on entity ID, message ID, file ID, and type
	extension := mediaItem.Extension
	if extension == "" {
		extension = getDefaultExtension(mediaItem.Type, mediaItem.MimeType)
	}

	// Use safer characters for filename parts
	safeID := utils.SanitizeFilename(entityID, 30, "_")
	safeFileID := utils.SanitizeFilename(mediaItem.FileID, 30, "_")
	
	filename := fmt.Sprintf("%s_%d_%s_%s.%s",
		safeID,
		messageID,
		safeFileID,
		mediaItem.Type,
		extension)

	return utils.SanitizeFilename(filename, 200, "_")
}

// getDefaultExtension returns a default extension for a media type
func getDefaultExtension(mediaType, mimeType string) string {
	switch mediaType {
	case "photo":
		return "jpg"
	case "video":
		return "mp4"
	case "audio":
		return "mp3"
	case "voice":
		return "ogg"
	case "animation":
		return "gif"
	case "document":
		// Try to get extension from MIME type
		if mimeType != "" {
			parts := strings.Split(mimeType, "/")
			if len(parts) == 2 {
				return parts[1]
			}
		}
		return "bin"
	default:
		return "bin"
	}
}

// optimizeMedia optimizes media based on its type
func (mp *MediaProcessor) optimizeMedia(rawPath string, finalPath string, mediaItem *telegram.MediaItem) error {
	// Check if the original file exists
	if _, err := os.Stat(rawPath); os.IsNotExist(err) {
		return fmt.Errorf("source file does not exist: %s", rawPath)
	}

	// Get raw file stats for later comparison
	rawInfo, err := os.Stat(rawPath)
	if err != nil {
		return fmt.Errorf("failed to get raw file stats: %v", err)
	}
	rawSize := rawInfo.Size()

	// If file is too small (less than 5KB) or ffmpeg not available, just copy it as is
	if rawSize < 5*1024 || !mp.ffmpegAvailable || !mp.config.MediaDownload {
		utils.LogDebug("Skipping optimization: too small or ffmpeg not available")
		return os.Rename(rawPath, finalPath)
	}

	// Perform optimization based on media type
	switch mediaItem.Type {
	case "photo":
		return mp.optimizeImage(rawPath, finalPath)
	case "video":
		return mp.optimizeVideo(rawPath, finalPath, mediaItem.Width, mediaItem.Height, mediaItem.Duration)
	case "audio":
		return mp.optimizeAudio(rawPath, finalPath, false)
	case "voice":
		return mp.optimizeAudio(rawPath, finalPath, true)
	case "animation":
		// For animations (GIFs), try to convert to WebP if it's under a certain size
		if rawSize < 4*1024*1024 { // 4MB
			if err := mp.optimizeGIF(rawPath, finalPath); err == nil {
				return nil
			}
			// If optimization fails, fall back to copying
			utils.LogDebug("GIF optimization failed, falling back to copy")
		}
		return os.Rename(rawPath, finalPath)
	default:
		// For other types, just copy the file
		return os.Rename(rawPath, finalPath)
	}
}

// optimizeImage optimizes an image
func (mp *MediaProcessor) optimizeImage(inputPath string, outputPath string) error {
	// Determine output format
	outputExt := filepath.Ext(outputPath)
	if outputExt == "" {
		outputExt = ".webp" // Default to WebP for best compression
		outputPath = outputPath + outputExt
	}

	var args []string
	
	// Use different encoding based on output format
	switch strings.ToLower(outputExt) {
	case ".webp":
		// Convert to WebP with specified quality
		args = []string{
			"-i", inputPath,
			"-q:v", fmt.Sprintf("%d", mp.config.ImageQuality),
			"-c:v", "libwebp",
			"-lossless", "0",
			"-compression_level", "6",
			"-y", // Overwrite output files without asking
			outputPath,
		}
	case ".jpg", ".jpeg":
		// Optimize JPEG
		args = []string{
			"-i", inputPath,
			"-q:v", fmt.Sprintf("%d", mp.config.ImageQuality),
			"-y",
			outputPath,
		}
	case ".png":
		// Convert to PNG
		args = []string{
			"-i", inputPath,
			"-y",
			outputPath,
		}
	default:
		// Default conversion with quality setting
		args = []string{
			"-i", inputPath,
			"-q:v", fmt.Sprintf("%d", mp.config.ImageQuality),
			"-y",
			outputPath,
		}
	}

	cmd := exec.Command("ffmpeg", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to optimize image: %v\nOutput: %s", err, output)
	}

	// Check if the optimized file is smaller
	inputInfo, err := os.Stat(inputPath)
	if err != nil {
		return fmt.Errorf("failed to stat input file: %v", err)
	}

	outputInfo, err := os.Stat(outputPath)
	if err != nil {
		return fmt.Errorf("failed to stat output file: %v", err)
	}

	// If the optimized file is larger, use the original
	if outputInfo.Size() >= inputInfo.Size() {
		utils.LogDebug("Optimized image is larger than original, using original")
		os.Remove(outputPath)
		return os.Rename(inputPath, outputPath)
	}

	return nil
}

// optimizeGIF optimizes a GIF, converting it to WebP if possible
func (mp *MediaProcessor) optimizeGIF(inputPath string, outputPath string) error {
	// Change extension to webp
	outputPathWebP := strings.TrimSuffix(outputPath, filepath.Ext(outputPath)) + ".webp"

	// Convert GIF to animated WebP
	args := []string{
		"-i", inputPath,
		"-c:v", "libwebp",
		"-lossless", "0",
		"-q:v", fmt.Sprintf("%d", mp.config.ImageQuality),
		"-loop", "0", // Keep original loop count
		"-y",
		outputPathWebP,
	}

	cmd := exec.Command("ffmpeg", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(outputPathWebP) // Clean up partial file
		return fmt.Errorf("failed to optimize GIF: %v\nOutput: %s", err, output)
	}

	// Check if the optimized file is smaller
	inputInfo, err := os.Stat(inputPath)
	if err != nil {
		return fmt.Errorf("failed to stat input file: %v", err)
	}

	outputInfo, err := os.Stat(outputPathWebP)
	if err != nil {
		return fmt.Errorf("failed to stat output file: %v", err)
	}

	// If the optimized WebP is smaller, use it
	if outputInfo.Size() < inputInfo.Size() {
		// If the output path expected a different extension, rename the file
		if outputPathWebP != outputPath {
			if err := os.Rename(outputPathWebP, outputPath); err != nil {
				return fmt.Errorf("failed to rename optimized WebP: %v", err)
			}
		}
		return nil
	}

	// Otherwise, use the original GIF
	os.Remove(outputPathWebP)
	return os.Rename(inputPath, outputPath)
}

// optimizeVideo optimizes a video
func (mp *MediaProcessor) optimizeVideo(inputPath string, outputPath string, width, height, duration int) error {
	// Check input file size
	inputInfo, err := os.Stat(inputPath)
	if err != nil {
		return fmt.Errorf("failed to stat input file: %v", err)
	}

	// For small videos or short clips (less than 10MB or 10 seconds), use higher quality settings
	useFastSettings := inputInfo.Size() < 10*1024*1024 || duration <= 10

	// Configure encoder
	videoCodec := "libx264" // Default to H.264
	if mp.config.UseH265 {
		videoCodec = "libx265" // Use H.265 if configured
	}

	// Determine video quality
	crf := mp.config.VideoCRF
	if useFastSettings {
		// Use better quality for small videos
		crf = max(crf-4, 18)
	}

	// Configure hardware acceleration
	hwAccel := ""
	if mp.config.HWAcceleration != "none" {
		switch mp.config.HWAcceleration {
		case "nvidia":
			if mp.config.UseH265 {
				videoCodec = "hevc_nvenc"
			} else {
				videoCodec = "h264_nvenc"
			}
			hwAccel = "-hwaccel cuda"
		case "amd":
			if mp.config.UseH265 {
				videoCodec = "hevc_amf"
			} else {
				videoCodec = "h264_amf"
			}
			hwAccel = "-hwaccel d3d11va"
		case "intel":
			if mp.config.UseH265 {
				videoCodec = "hevc_qsv"
			} else {
				videoCodec = "h264_qsv"
			}
			hwAccel = "-hwaccel qsv"
		}
	}

	// Build the command
	args := []string{
		"-i", inputPath,
		"-c:v", videoCodec,
		"-crf", fmt.Sprintf("%d", crf),
		"-preset", mp.config.VideoPreset,
	}

	// Add scale if we have valid dimensions and they're large
	if width > 0 && height > 0 && (width > 1280 || height > 720) {
		// Calculate new dimensions, maintaining aspect ratio
		newWidth, newHeight := calculateNewDimensions(width, height, 1280, 720)
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d", newWidth, newHeight))
	}

	// Audio settings
	args = append(args, "-c:a", "aac", "-b:a", "128k")

	// Output file
	args = append(args, "-y", outputPath)

	if hwAccel != "" {
		args = append([]string{hwAccel}, args...)
	}

	cmd := exec.Command("ffmpeg", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		utils.LogWarn("Failed to optimize video: %v\nCommand: ffmpeg %s\nOutput: %s", 
			err, strings.Join(args, " "), output)
		return os.Rename(inputPath, outputPath)
	}

	// Check if the optimized file is smaller
	outputInfo, err := os.Stat(outputPath)
	if err != nil {
		return fmt.Errorf("failed to stat output file: %v", err)
	}

	// If the optimized file is larger, use the original
	if outputInfo.Size() >= inputInfo.Size() {
		os.Remove(outputPath)
		return os.Rename(inputPath, outputPath)
	}

	return nil
}

// calculateNewDimensions calculates new dimensions while maintaining aspect ratio
func calculateNewDimensions(originalWidth, originalHeight, maxWidth, maxHeight int) (int, int) {
	if originalWidth <= maxWidth && originalHeight <= maxHeight {
		return originalWidth, originalHeight
	}

	widthRatio := float64(maxWidth) / float64(originalWidth)
	heightRatio := float64(maxHeight) / float64(originalHeight)

	// Use the smaller ratio to ensure both dimensions fit within the max
	ratio := min(widthRatio, heightRatio)

	newWidth := int(float64(originalWidth) * ratio)
	newHeight := int(float64(originalHeight) * ratio)

	// Ensure dimensions are even (required by some codecs)
	if newWidth % 2 != 0 {
		newWidth -= 1
	}
	if newHeight % 2 != 0 {
		newHeight -= 1
	}

	return newWidth, newHeight
}

// optimizeAudio optimizes an audio file
func (mp *MediaProcessor) optimizeAudio(inputPath string, outputPath string, isVoice bool) error {
	// Configure codec and bitrate based on if it's voice or music
	codec := "libopus"
	bitrate := "64k" // Default for voice

	if !isVoice {
		bitrate = "128k" // Higher bitrate for music
	}

	// Build the command
	args := []string{
		"-i", inputPath,
		"-c:a", codec,
		"-b:a", bitrate,
		"-y", // Overwrite output files without asking
		outputPath,
	}

	// Add voice-specific optimizations
	if isVoice {
		// Use mono for voice recordings, apply some noise reduction
		args = append(args, "-ac", "1", "-af", "highpass=f=200,lowpass=f=3000")
	}

	cmd := exec.Command("ffmpeg", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to optimize audio: %v\nOutput: %s", err, output)
	}

	// Check if the optimized file is smaller
	inputInfo, err := os.Stat(inputPath)
	if err != nil {
		return fmt.Errorf("failed to stat input file: %v", err)
	}

	outputInfo, err := os.Stat(outputPath)
	if err != nil {
		return fmt.Errorf("failed to stat output file: %v", err)
	}

	// If the optimized file is larger, use the original
	if outputInfo.Size() >= inputInfo.Size() {
		os.Remove(outputPath)
		return os.Rename(inputPath, outputPath)
	}

	return nil
}

// min returns the smaller of two float64 values
func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// max returns the larger of two int values
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
