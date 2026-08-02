package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port               string
	ProjectID          string
	Location           string
	ModelID            string
	DevMock            bool
	MaxRequestBytes    int64
	MaxMessages        int
	MaxConcurrentChats int
	RequestTimeout     time.Duration
	AllowedOrigins     []string

	AuthToken                   string
	DBPath                      string
	CookieSecure                bool
	MaxUploadBytes              int64
	MaxDocumentContextChars     int
	MaxContextMessages          int
	MaxConversationsPerSession  int
	MaxDocumentsPerConversation int
	MaxSavedPromptsPerSession   int
}

func Load() (Config, error) {
	cfg := Config{
		Port:               envOr("PORT", "8080"),
		ProjectID:          strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT")),
		Location:           envOr("GOOGLE_CLOUD_LOCATION", "global"),
		ModelID:            envOr("MODEL_ID", "gemini-3.5-flash"),
		MaxRequestBytes:    2 << 20,
		MaxMessages:        100,
		MaxConcurrentChats: 32,
		RequestTimeout:     5 * time.Minute,

		AuthToken:                   strings.TrimSpace(os.Getenv("AUTH_TOKEN")),
		DBPath:                      envOr("DB_PATH", "./data/vertex-chat.db"),
		MaxUploadBytes:              20 << 20,
		MaxDocumentContextChars:     24000,
		MaxContextMessages:          40,
		MaxConversationsPerSession:  200,
		MaxDocumentsPerConversation: 20,
		MaxSavedPromptsPerSession:   100,
	}

	var err error
	if cfg.DevMock, err = boolEnv("DEV_MOCK", false); err != nil {
		return Config{}, err
	}
	if cfg.MaxRequestBytes, err = int64Env("MAX_REQUEST_BYTES", cfg.MaxRequestBytes); err != nil {
		return Config{}, err
	}
	if cfg.MaxMessages, err = intEnv("MAX_MESSAGES", cfg.MaxMessages); err != nil {
		return Config{}, err
	}
	if cfg.MaxConcurrentChats, err = intEnv("MAX_CONCURRENT_CHATS", cfg.MaxConcurrentChats); err != nil {
		return Config{}, err
	}
	if cfg.RequestTimeout, err = durationEnv("REQUEST_TIMEOUT", cfg.RequestTimeout); err != nil {
		return Config{}, err
	}
	if cfg.CookieSecure, err = boolEnv("COOKIE_SECURE", true); err != nil {
		return Config{}, err
	}
	if cfg.MaxUploadBytes, err = int64Env("MAX_UPLOAD_BYTES", cfg.MaxUploadBytes); err != nil {
		return Config{}, err
	}
	if cfg.MaxDocumentContextChars, err = intEnv("MAX_DOCUMENT_CONTEXT_CHARS", cfg.MaxDocumentContextChars); err != nil {
		return Config{}, err
	}
	if cfg.MaxContextMessages, err = intEnv("MAX_CONTEXT_MESSAGES", cfg.MaxContextMessages); err != nil {
		return Config{}, err
	}
	if cfg.MaxConversationsPerSession, err = intEnv("MAX_CONVERSATIONS_PER_SESSION", cfg.MaxConversationsPerSession); err != nil {
		return Config{}, err
	}
	if cfg.MaxDocumentsPerConversation, err = intEnv("MAX_DOCUMENTS_PER_CONVERSATION", cfg.MaxDocumentsPerConversation); err != nil {
		return Config{}, err
	}
	if cfg.MaxSavedPromptsPerSession, err = intEnv("MAX_SAVED_PROMPTS_PER_SESSION", cfg.MaxSavedPromptsPerSession); err != nil {
		return Config{}, err
	}

	if raw := strings.TrimSpace(os.Getenv("ALLOWED_ORIGINS")); raw != "" {
		for _, origin := range strings.Split(raw, ",") {
			if origin = strings.TrimSpace(origin); origin != "" {
				cfg.AllowedOrigins = append(cfg.AllowedOrigins, origin)
			}
		}
	}

	if cfg.Port == "" {
		return Config{}, fmt.Errorf("PORT must not be empty")
	}
	if cfg.ProjectID == "" && !cfg.DevMock {
		return Config{}, fmt.Errorf("GOOGLE_CLOUD_PROJECT is required unless DEV_MOCK=true")
	}
	if cfg.ModelID == "" {
		return Config{}, fmt.Errorf("MODEL_ID must not be empty")
	}
	if cfg.MaxRequestBytes < 1024 {
		return Config{}, fmt.Errorf("MAX_REQUEST_BYTES must be at least 1024")
	}
	if cfg.MaxMessages < 1 {
		return Config{}, fmt.Errorf("MAX_MESSAGES must be greater than zero")
	}
	if cfg.MaxConcurrentChats < 1 {
		return Config{}, fmt.Errorf("MAX_CONCURRENT_CHATS must be greater than zero")
	}
	if cfg.RequestTimeout <= 0 {
		return Config{}, fmt.Errorf("REQUEST_TIMEOUT must be greater than zero")
	}
	if cfg.AuthToken == "" {
		return Config{}, fmt.Errorf("AUTH_TOKEN is required")
	}
	if cfg.DBPath == "" {
		return Config{}, fmt.Errorf("DB_PATH must not be empty")
	}
	if cfg.MaxUploadBytes < 1024 {
		return Config{}, fmt.Errorf("MAX_UPLOAD_BYTES must be at least 1024")
	}
	if cfg.MaxDocumentContextChars < 1 {
		return Config{}, fmt.Errorf("MAX_DOCUMENT_CONTEXT_CHARS must be greater than zero")
	}
	if cfg.MaxContextMessages < 1 {
		return Config{}, fmt.Errorf("MAX_CONTEXT_MESSAGES must be greater than zero")
	}
	if cfg.MaxConversationsPerSession < 1 {
		return Config{}, fmt.Errorf("MAX_CONVERSATIONS_PER_SESSION must be greater than zero")
	}
	if cfg.MaxDocumentsPerConversation < 1 {
		return Config{}, fmt.Errorf("MAX_DOCUMENTS_PER_CONVERSATION must be greater than zero")
	}
	if cfg.MaxSavedPromptsPerSession < 1 {
		return Config{}, fmt.Errorf("MAX_SAVED_PROMPTS_PER_SESSION must be greater than zero")
	}

	return cfg, nil
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func boolEnv(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", key, err)
	}
	return value, nil
}

func intEnv(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return value, nil
}

func int64Env(key string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return value, nil
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as 5m: %w", key, err)
	}
	return value, nil
}
