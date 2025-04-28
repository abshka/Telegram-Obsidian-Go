package utils

import (
	"fmt"
)

// BaseError is the base error type for the application
type BaseError struct {
	message string
	cause   error
}

// Error returns the error message
func (e *BaseError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %v", e.message, e.cause)
	}
	return e.message
}

// Unwrap returns the underlying error
func (e *BaseError) Unwrap() error {
	return e.cause
}

// NewError creates a new error
func NewError(message string, cause error) error {
	return &BaseError{
		message: message,
		cause:   cause,
	}
}

// ConfigError represents a configuration error
type ConfigError struct {
	BaseError
}

// NewConfigError creates a new configuration error
func NewConfigError(message string, cause error) error {
	return &ConfigError{
		BaseError: BaseError{
			message: message,
			cause:   cause,
		},
	}
}

// TelegramError represents a Telegram API error
type TelegramError struct {
	BaseError
}

// NewTelegramError creates a new Telegram error
func NewTelegramError(message string, cause error) error {
	return &TelegramError{
		BaseError: BaseError{
			message: message,
			cause:   cause,
		},
	}
}

// CacheError represents a cache operation error
type CacheError struct {
	BaseError
}

// NewCacheError creates a new cache error
func NewCacheError(message string, cause error) error {
	return &CacheError{
		BaseError: BaseError{
			message: message,
			cause:   cause,
		},
	}
}

// MediaError represents a media processing error
type MediaError struct {
	BaseError
}

// NewMediaError creates a new media error
func NewMediaError(message string, cause error) error {
	return &MediaError{
		BaseError: BaseError{
			message: message,
			cause:   cause,
		},
	}
}

// NoteError represents a note creation error
type NoteError struct {
	BaseError
}

// NewNoteError creates a new note error
func NewNoteError(message string, cause error) error {
	return &NoteError{
		BaseError: BaseError{
			message: message,
			cause:   cause,
		},
	}
}