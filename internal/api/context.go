package api

import (
	"context"
	"errors"
	"strings"

	"vertex-chat/internal/chat"
	"vertex-chat/internal/session"
	"vertex-chat/internal/store"
)

// resolveConversation returns the conversation referenced by
// conversationID (a client-supplied thread id — the OpenAI-compatible
// endpoint has no other way to hand one back before the first call),
// creating a row under that exact id if it doesn't resolve, so the id the
// caller sent is the id it gets back on every subsequent call. When
// sessionID is non-empty the lookup (and any newly created conversation)
// is scoped to that session; when empty — a cookie-less caller already
// authenticated via the shared token — the lookup is unscoped and, only if
// a fresh conversation actually needs to be created, a throwaway internal
// session is minted to satisfy the schema's foreign key. That session is
// never exposed via a cookie since there's no browser to persist it.
func (h *Handler) resolveConversation(ctx context.Context, sessionID, conversationID string) (store.Conversation, error) {
	var conv store.Conversation
	var err error
	if sessionID != "" {
		conv, err = h.store.GetConversation(ctx, conversationID, sessionID)
	} else {
		conv, err = h.store.GetConversationUnscoped(ctx, conversationID)
	}
	if err == nil {
		return conv, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Conversation{}, err
	}

	ownerSession := sessionID
	if ownerSession == "" {
		token, err := session.NewToken()
		if err != nil {
			return store.Conversation{}, err
		}
		if _, err := h.store.CreateSession(ctx, token); err != nil {
			return store.Conversation{}, err
		}
		ownerSession = token
	}
	return h.store.CreateConversationWithID(ctx, conversationID, ownerSession, "", "")
}

// mergeMessages prepends stored history to the client-supplied fresh
// turn(s), trimming from the stored head first when over budget — the
// client's new turn is never dropped.
func mergeMessages(stored, fresh []chat.Message, max int) []chat.Message {
	if len(fresh) >= max {
		return fresh
	}
	budget := max - len(fresh)
	if len(stored) > budget {
		stored = stored[len(stored)-budget:]
	}
	merged := make([]chat.Message, 0, len(stored)+len(fresh))
	merged = append(merged, stored...)
	merged = append(merged, fresh...)
	return merged
}

// deriveTitle produces a short conversation title from the first user
// message, used when a conversation is auto-created without an explicit
// title.
func deriveTitle(text string) string {
	text = strings.TrimSpace(strings.Join(strings.Fields(text), " "))
	const max = 60
	if len(text) <= max {
		return text
	}
	return text[:max] + "…"
}
