package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"vertex-chat/internal/chat"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSessionCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if sess.ID != "sess-1" {
		t.Fatalf("ID = %q, want sess-1", sess.ID)
	}

	got, err := s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if got.ID != "sess-1" {
		t.Fatalf("ID = %q, want sess-1", got.ID)
	}

	if _, err := s.GetSession(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("GetSession(missing) error = %v, want ErrNotFound", err)
	}
}

func TestConversationCRUDAndSessionScoping(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.CreateSession(ctx, "sess-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(ctx, "sess-b"); err != nil {
		t.Fatal(err)
	}

	conv, err := s.CreateConversation(ctx, "sess-a", "My Chat", "be helpful")
	if err != nil {
		t.Fatalf("CreateConversation() error = %v", err)
	}

	got, err := s.GetConversation(ctx, conv.ID, "sess-a")
	if err != nil {
		t.Fatalf("GetConversation() error = %v", err)
	}
	if got.Title != "My Chat" {
		t.Fatalf("Title = %q, want %q", got.Title, "My Chat")
	}

	if _, err := s.GetConversation(ctx, conv.ID, "sess-b"); err != ErrNotFound {
		t.Fatalf("GetConversation() cross-session error = %v, want ErrNotFound", err)
	}

	if _, err := s.GetConversationUnscoped(ctx, conv.ID); err != nil {
		t.Fatalf("GetConversationUnscoped() error = %v", err)
	}

	if err := s.RenameConversation(ctx, conv.ID, "sess-a", "Renamed"); err != nil {
		t.Fatalf("RenameConversation() error = %v", err)
	}
	got, _ = s.GetConversation(ctx, conv.ID, "sess-a")
	if got.Title != "Renamed" {
		t.Fatalf("Title after rename = %q, want Renamed", got.Title)
	}

	if err := s.RenameConversation(ctx, conv.ID, "sess-b", "Hijacked"); err != ErrNotFound {
		t.Fatalf("RenameConversation() cross-session error = %v, want ErrNotFound", err)
	}

	if err := s.UpdateSystemInstruction(ctx, conv.ID, "sess-a", "new system"); err != nil {
		t.Fatalf("UpdateSystemInstruction() error = %v", err)
	}
	got, _ = s.GetConversation(ctx, conv.ID, "sess-a")
	if got.SystemInstruction != "new system" {
		t.Fatalf("SystemInstruction = %q, want %q", got.SystemInstruction, "new system")
	}

	list, err := s.ListConversations(ctx, "sess-a", 10)
	if err != nil {
		t.Fatalf("ListConversations() error = %v", err)
	}
	if len(list) != 1 || list[0].ID != conv.ID {
		t.Fatalf("ListConversations() = %+v, want single entry for %q", list, conv.ID)
	}

	n, err := s.CountConversations(ctx, "sess-a")
	if err != nil || n != 1 {
		t.Fatalf("CountConversations() = %d, %v, want 1, nil", n, err)
	}

	if err := s.DeleteConversation(ctx, conv.ID, "sess-b"); err != ErrNotFound {
		t.Fatalf("DeleteConversation() cross-session error = %v, want ErrNotFound", err)
	}
	if err := s.DeleteConversation(ctx, conv.ID, "sess-a"); err != nil {
		t.Fatalf("DeleteConversation() error = %v", err)
	}
	if _, err := s.GetConversation(ctx, conv.ID, "sess-a"); err != ErrNotFound {
		t.Fatalf("GetConversation() after delete error = %v, want ErrNotFound", err)
	}
}

func TestMessagesAppendAndRecent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.CreateSession(ctx, "sess-1"); err != nil {
		t.Fatal(err)
	}
	conv, err := s.CreateConversation(ctx, "sess-1", "", "")
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		msgs := []chat.Message{
			{Role: "user", Content: "question"},
			{Role: "assistant", Content: "answer"},
		}
		if err := s.AppendMessages(ctx, conv.ID, msgs); err != nil {
			t.Fatalf("AppendMessages() error = %v", err)
		}
	}

	recent, err := s.RecentMessages(ctx, conv.ID, 4)
	if err != nil {
		t.Fatalf("RecentMessages() error = %v", err)
	}
	if len(recent) != 4 {
		t.Fatalf("len(recent) = %d, want 4", len(recent))
	}
	if recent[0].Role != "user" || recent[len(recent)-1].Role != "assistant" {
		t.Fatalf("recent = %+v, want oldest-first user/assistant pairs", recent)
	}

	all, err := s.RecentMessages(ctx, conv.ID, 100)
	if err != nil {
		t.Fatalf("RecentMessages() error = %v", err)
	}
	if len(all) != 10 {
		t.Fatalf("len(all) = %d, want 10", len(all))
	}
}

func TestDocumentsCRUDAndContext(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.CreateSession(ctx, "sess-1"); err != nil {
		t.Fatal(err)
	}
	conv, err := s.CreateConversation(ctx, "sess-1", "", "")
	if err != nil {
		t.Fatal(err)
	}

	doc, err := s.CreateDocument(ctx, conv.ID, "sess-1", "a.pdf", "application/pdf", 1024)
	if err != nil {
		t.Fatalf("CreateDocument() error = %v", err)
	}
	if doc.Status != "processing" {
		t.Fatalf("Status = %q, want processing", doc.Status)
	}

	if err := s.MarkDocumentReady(ctx, doc.ID, "extracted text from a.pdf"); err != nil {
		t.Fatalf("MarkDocumentReady() error = %v", err)
	}

	doc2, err := s.CreateDocument(ctx, conv.ID, "sess-1", "b.txt", "text/plain", 512)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDocumentFailed(ctx, doc2.ID, "boom"); err != nil {
		t.Fatalf("MarkDocumentFailed() error = %v", err)
	}

	got, err := s.GetDocument(ctx, doc.ID, conv.ID)
	if err != nil {
		t.Fatalf("GetDocument() error = %v", err)
	}
	if got.Status != "ready" || got.ExtractedText != "extracted text from a.pdf" {
		t.Fatalf("GetDocument() = %+v, want ready with extracted text", got)
	}

	list, err := s.ListDocuments(ctx, conv.ID)
	if err != nil {
		t.Fatalf("ListDocuments() error = %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len(list) = %d, want 2", len(list))
	}

	n, err := s.CountDocuments(ctx, conv.ID)
	if err != nil || n != 2 {
		t.Fatalf("CountDocuments() = %d, %v, want 2, nil", n, err)
	}

	docCtx, err := s.DocumentsContext(ctx, conv.ID, 1000)
	if err != nil {
		t.Fatalf("DocumentsContext() error = %v", err)
	}
	if !strings.Contains(docCtx, "### a.pdf") || !strings.Contains(docCtx, "extracted text from a.pdf") {
		t.Fatalf("DocumentsContext() = %q, missing ready document content", docCtx)
	}
	if strings.Contains(docCtx, "b.txt") {
		t.Fatalf("DocumentsContext() = %q, should not include failed document", docCtx)
	}

	truncated, err := s.DocumentsContext(ctx, conv.ID, 5)
	if err != nil {
		t.Fatalf("DocumentsContext() error = %v", err)
	}
	if len(truncated) != 5 {
		t.Fatalf("len(truncated) = %d, want 5", len(truncated))
	}

	if err := s.DeleteDocument(ctx, doc.ID, conv.ID); err != nil {
		t.Fatalf("DeleteDocument() error = %v", err)
	}
	if _, err := s.GetDocument(ctx, doc.ID, conv.ID); err != ErrNotFound {
		t.Fatalf("GetDocument() after delete error = %v, want ErrNotFound", err)
	}
}

func TestSavedPromptsCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.CreateSession(ctx, "sess-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(ctx, "sess-2"); err != nil {
		t.Fatal(err)
	}

	p, err := s.CreateSavedPrompt(ctx, "sess-1", "Greeting", "Say hello")
	if err != nil {
		t.Fatalf("CreateSavedPrompt() error = %v", err)
	}

	if _, err := s.GetSavedPrompt(ctx, p.ID, "sess-2"); err != ErrNotFound {
		t.Fatalf("GetSavedPrompt() cross-session error = %v, want ErrNotFound", err)
	}

	if err := s.UpdateSavedPrompt(ctx, p.ID, "sess-1", "Greeting v2", "Say hi"); err != nil {
		t.Fatalf("UpdateSavedPrompt() error = %v", err)
	}
	got, err := s.GetSavedPrompt(ctx, p.ID, "sess-1")
	if err != nil || got.Title != "Greeting v2" {
		t.Fatalf("GetSavedPrompt() = %+v, %v, want Greeting v2", got, err)
	}

	list, err := s.ListSavedPrompts(ctx, "sess-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListSavedPrompts() = %+v, %v, want 1 entry", list, err)
	}

	n, err := s.CountSavedPrompts(ctx, "sess-1")
	if err != nil || n != 1 {
		t.Fatalf("CountSavedPrompts() = %d, %v, want 1, nil", n, err)
	}

	if err := s.DeleteSavedPrompt(ctx, p.ID, "sess-2"); err != ErrNotFound {
		t.Fatalf("DeleteSavedPrompt() cross-session error = %v, want ErrNotFound", err)
	}
	if err := s.DeleteSavedPrompt(ctx, p.ID, "sess-1"); err != nil {
		t.Fatalf("DeleteSavedPrompt() error = %v", err)
	}
}

func TestCascadeDeleteSessionRemovesEverything(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.CreateSession(ctx, "sess-1"); err != nil {
		t.Fatal(err)
	}
	conv, err := s.CreateConversation(ctx, "sess-1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessages(ctx, conv.ID, []chat.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	doc, err := s.CreateDocument(ctx, conv.ID, "sess-1", "a.pdf", "application/pdf", 10)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, "sess-1"); err != nil {
		t.Fatalf("delete session: %v", err)
	}

	if _, err := s.GetConversationUnscoped(ctx, conv.ID); err != ErrNotFound {
		t.Fatalf("conversation survived session cascade delete: err = %v", err)
	}
	var msgCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, conv.ID).Scan(&msgCount); err != nil {
		t.Fatal(err)
	}
	if msgCount != 0 {
		t.Fatalf("messages survived cascade delete: count = %d", msgCount)
	}
	if _, err := s.GetDocument(ctx, doc.ID, conv.ID); err != ErrNotFound {
		t.Fatalf("document survived cascade delete: err = %v", err)
	}
}
