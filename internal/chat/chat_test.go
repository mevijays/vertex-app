package chat

import (
	"context"
	"strings"
	"testing"
)

func TestMockDocumentAnalyzer(t *testing.T) {
	text, err := MockDocumentAnalyzer{}.AnalyzeDocument(context.Background(), []byte("%PDF-1.4 fake"), "application/pdf", "report.pdf")
	if err != nil {
		t.Fatalf("AnalyzeDocument() error = %v", err)
	}
	if !strings.Contains(text, "report.pdf") || !strings.Contains(text, "application/pdf") {
		t.Fatalf("AnalyzeDocument() = %q, want it to mention filename and mime type", text)
	}
}

func TestMockDocumentAnalyzerRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (MockDocumentAnalyzer{}).AnalyzeDocument(ctx, nil, "text/plain", "a.txt"); err == nil {
		t.Fatal("AnalyzeDocument() with canceled context: error = nil, want context.Canceled")
	}
}
